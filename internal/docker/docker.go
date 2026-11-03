// Package docker talks to the Docker Engine API over its unix socket. A small HTTP
// client is enough, and it avoids the large dependency tree of the official SDK.
package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
)

const daemonConfig = "/etc/docker/daemon.json"

var (
	validID  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/@-]*$`)
	validReg = regexp.MustCompile(`^https?://[a-zA-Z0-9.-]+(:\d+)?(/[a-zA-Z0-9._/-]*)?$`)
)

type Docker struct {
	app    *core.App
	client *http.Client

	statsMu sync.Mutex
	lastCPU map[string]cpuSample // previous reading per container, for computing CPU use
}

func New(app *core.App) *Docker {
	return &Docker{app: app, client: NewClient(), lastCPU: map[string]cpuSample{}}
}

// NewClient returns an HTTP client bound to the docker socket.
func NewClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", "/var/run/docker.sock")
		},
	}}
}

func (d *Docker) Name() string { return "docker" }

func (d *Docker) Routes(r chi.Router) {
	r.Get("/docker/containers", d.proxyGet("/containers/json?all=1"))
	r.Get("/docker/containers/{id}/inspect", d.inspect)
	r.Get("/docker/containers/{id}/logs", d.logs)
	r.Post("/docker/containers/{id}/logs/vacuum", d.logsVacuum)
	r.Get("/docker/stats", d.statsAll)
	r.Post("/docker/containers/{id}/{action:start|stop|restart}", d.containerAction)
	r.Delete("/docker/containers/{id}", d.containerRemove)

	r.Get("/docker/images", d.proxyGet("/images/json"))
	r.Post("/docker/images/pull", d.imagePull)
	r.Post("/docker/images/prune", d.imagePrune)
	r.Delete("/docker/images", d.imageRemove)

	r.Get("/docker/networks", d.proxyGet("/networks"))
	r.Post("/docker/networks", d.networkCreate)
	r.Delete("/docker/networks/{id}", d.networkRemove)

	r.Get("/docker/volumes", d.proxyGet("/volumes"))
	r.Delete("/docker/volumes/{id}", d.volumeRemove)

	r.Get("/docker/mirrors", d.mirrorsGet)
	r.Post("/docker/mirrors", d.mirrorsSet)
}

// call performs an Engine API request. Non-2xx responses become errors carrying the
// daemon's own message.
func (d *Docker) call(ctx context.Context, method, path string, body io.Reader) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		var e struct{ Message string }
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			return nil, fmt.Errorf("%s", e.Message)
		}
		return nil, fmt.Errorf("docker: HTTP %d", resp.StatusCode)
	}
	return data, nil
}

func (d *Docker) proxyGet(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := d.call(r.Context(), http.MethodGet, path, nil)
		if err != nil {
			core.Fail(w, http.StatusBadGateway, "%v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}
}

// id extracts and validates a path parameter, replying 400 itself when invalid.
func id(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := chi.URLParam(r, "id")
	if !validID.MatchString(v) {
		core.Fail(w, http.StatusBadRequest, "invalid id")
		return "", false
	}
	return url.PathEscape(v), true
}

// inspect hides environment variables: they routinely hold passwords, and the
// Containers page has no need to display them.
func (d *Docker) inspect(w http.ResponseWriter, r *http.Request) {
	cid, ok := id(w, r)
	if !ok {
		return
	}
	data, err := d.call(r.Context(), http.MethodGet, "/containers/"+cid+"/json", nil)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	if cfg, ok := v["Config"].(map[string]any); ok {
		if env, ok := cfg["Env"].([]any); ok {
			for i, e := range env {
				if s, ok := e.(string); ok {
					name, _, _ := strings.Cut(s, "=")
					env[i] = name + "=***"
				}
			}
		}
	}
	core.OK(w, v)
}

func (d *Docker) logs(w http.ResponseWriter, r *http.Request) {
	cid, ok := id(w, r)
	if !ok {
		return
	}
	tail, err := strconv.Atoi(r.URL.Query().Get("tail"))
	if err != nil || tail <= 0 || tail > 10000 {
		tail = 500
	}
	data, err := d.call(r.Context(), http.MethodGet, fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&timestamps=1&tail=%d", cid, tail), nil)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	size, _ := d.logSize(r.Context(), cid)
	core.OK(w, map[string]any{"logs": demux(data), "size": size})
}

// logFile returns where docker keeps a container's log, checking it's where docker's
// json-file driver is expected to put it before anything reads or trims it.
func (d *Docker) logFile(ctx context.Context, cid string) (string, error) {
	data, err := d.call(ctx, http.MethodGet, "/containers/"+cid+"/json", nil)
	if err != nil {
		return "", err
	}
	var info struct{ LogPath string }
	if err := json.Unmarshal(data, &info); err != nil {
		return "", err
	}
	if !strings.HasPrefix(info.LogPath, "/var/lib/docker/containers/") {
		return "", fmt.Errorf("this container's log driver doesn't keep a local file")
	}
	return info.LogPath, nil
}

// logSize includes rotated files (name.log.1 and so on), if the container rotates.
func (d *Docker) logSize(ctx context.Context, cid string) (int64, error) {
	path, err := d.logFile(ctx, cid)
	if err != nil {
		return 0, err
	}
	matches, err := filepath.Glob(path + "*")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, f := range matches {
		if info, err := os.Stat(f); err == nil {
			total += info.Size()
		}
	}
	return total, nil
}

// logsVacuum stops the container while it trims. Docker keeps the log's size in memory, so
// editing the file under a running container, even with plain truncate, makes `docker logs`
// hang until the container restarts. It doesn't use the request's context: if the browser
// goes away part-way through, the container must still come back up.
func (d *Docker) logsVacuum(w http.ResponseWriter, r *http.Request) {
	cid, ok := id(w, r)
	if !ok {
		return
	}
	var req struct{ KeepMB int64 }
	if !core.Read(w, r, &req) {
		return
	}
	keep := req.KeepMB << 20
	if req.KeepMB < 0 || keep > core.MaxVacuumKeep {
		core.Fail(w, http.StatusBadRequest, "size to keep must be between 0 and %d MB", core.MaxVacuumKeep>>20)
		return
	}
	ctx := context.Background()
	path, err := d.logFile(ctx, cid)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	data, err := d.call(ctx, http.MethodGet, "/containers/"+cid+"/json", nil)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	var info struct{ State struct{ Running bool } }
	if err := json.Unmarshal(data, &info); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	if info.State.Running {
		if _, err := d.call(ctx, http.MethodPost, "/containers/"+cid+"/stop?t=20", nil); err != nil {
			core.Fail(w, http.StatusBadGateway, "couldn't stop the container, log left as it was: %v", err)
			return
		}
	}
	var freed int64
	rotated, _ := filepath.Glob(path + ".*")
	for _, f := range rotated {
		if st, err := os.Stat(f); err == nil && os.Remove(f) == nil {
			freed += st.Size()
		}
	}
	n, trimErr := core.TrimLog(path, keep)
	if info.State.Running {
		if _, err := d.call(ctx, http.MethodPost, "/containers/"+cid+"/start", nil); err != nil {
			core.Fail(w, http.StatusBadGateway, "log trimmed, but the container didn't start again: %v", err)
			return
		}
	}
	if trimErr != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", trimErr)
		return
	}
	d.app.Audit(r, "container.logs.vacuum", cid)
	size, _ := d.logSize(ctx, cid)
	core.OK(w, map[string]int64{"freed": freed + n, "size": size})
}

// demux strips docker's 8-byte stream headers from non-TTY container logs.
func demux(data []byte) string {
	var b strings.Builder
	for len(data) >= 8 && data[0] <= 2 && data[1] == 0 && data[2] == 0 && data[3] == 0 {
		n := int(data[4])<<24 | int(data[5])<<16 | int(data[6])<<8 | int(data[7])
		data = data[8:]
		if n > len(data) {
			n = len(data)
		}
		b.Write(data[:n])
		data = data[n:]
	}
	b.Write(data) // TTY containers have no headers
	return b.String()
}

type containerStats struct {
	CPU      float64 `json:"cpu"` // share of the whole host, 0-100
	MemUsed  uint64  `json:"memUsed"`
	MemLimit uint64  `json:"memLimit"`
}

// cpuSample is a pair of cumulative counters. CPU use is the change between two of them.
type cpuSample struct{ total, system uint64 }

// parseStats turns one Engine API stats document into memory figures and a CPU reading.
func parseStats(data []byte) (containerStats, cpuSample, error) {
	var s struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage uint64 `json:"system_cpu_usage"`
		} `json:"cpu_stats"`
		MemoryStats struct {
			Usage uint64 `json:"usage"`
			Limit uint64 `json:"limit"`
			Stats struct {
				InactiveFile uint64 `json:"inactive_file"`
			} `json:"stats"`
		} `json:"memory_stats"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return containerStats{}, cpuSample{}, err
	}
	out := containerStats{MemLimit: s.MemoryStats.Limit, MemUsed: s.MemoryStats.Usage}
	// docker stats also leaves out reclaimable page cache; it isn't really "in use".
	if s.MemoryStats.Stats.InactiveFile < out.MemUsed {
		out.MemUsed -= s.MemoryStats.Stats.InactiveFile
	}
	return out, cpuSample{s.CPUStats.CPUUsage.TotalUsage, s.CPUStats.SystemCPUUsage}, nil
}

// cpuPercent is the container's share of the whole host between two readings.
func cpuPercent(prev, cur cpuSample) float64 {
	// With no prior reading the counters only give an average since the container started.
	if prev.system == 0 || cur.total <= prev.total || cur.system <= prev.system {
		return 0 // the same goes for a restart, which resets the counter
	}
	return float64(cur.total-prev.total) / float64(cur.system-prev.system) * 100
}

// statsAll samples every running container, for the live columns. one-shot asks the daemon
// to answer at once rather than spend a second measuring each, so polling stays cheap on
// a live server. The first reading for a container therefore shows no CPU use yet.
func (d *Docker) statsAll(w http.ResponseWriter, r *http.Request) {
	data, err := d.call(r.Context(), http.MethodGet, "/containers/json", nil)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	var list []struct{ Id string }
	if err := json.Unmarshal(data, &list); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	out := make(map[string]containerStats, len(list))
	samples := make(map[string]cpuSample, len(list))
	var mu sync.Mutex
	var wg sync.WaitGroup
	limit := make(chan struct{}, 8)
	for _, c := range list {
		wg.Add(1)
		go func(cid string) {
			defer wg.Done()
			limit <- struct{}{}
			defer func() { <-limit }()
			raw, err := d.call(r.Context(), http.MethodGet, "/containers/"+cid+"/stats?stream=false&one-shot=true", nil)
			if err != nil {
				return // it may have just stopped; the row simply shows no figures
			}
			if st, sample, err := parseStats(raw); err == nil {
				mu.Lock()
				out[cid], samples[cid] = st, sample
				mu.Unlock()
			}
		}(c.Id)
	}
	wg.Wait()

	d.statsMu.Lock()
	for cid, sample := range samples {
		st := out[cid]
		st.CPU = cpuPercent(d.lastCPU[cid], sample)
		out[cid] = st
	}
	// Replacing the map also forgets containers that no longer run.
	d.lastCPU = samples
	d.statsMu.Unlock()
	core.OK(w, out)
}

func (d *Docker) containerAction(w http.ResponseWriter, r *http.Request) {
	cid, ok := id(w, r)
	if !ok {
		return
	}
	action := chi.URLParam(r, "action")
	if _, err := d.call(r.Context(), http.MethodPost, "/containers/"+cid+"/"+action, nil); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	d.app.Audit(r, "container."+action, cid)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Docker) containerRemove(w http.ResponseWriter, r *http.Request) {
	cid, ok := id(w, r)
	if !ok {
		return
	}
	if _, err := d.call(r.Context(), http.MethodDelete, "/containers/"+cid+"?force=1", nil); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	d.app.Audit(r, "container.remove", cid)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Docker) imagePull(w http.ResponseWriter, r *http.Request) {
	var req struct{ Image string }
	if !core.Read(w, r, &req) {
		return
	}
	if !validID.MatchString(req.Image) {
		core.Fail(w, http.StatusBadRequest, "invalid image name")
		return
	}
	task := d.app.Tasks.Start("Pull "+req.Image, func(_ context.Context, t *core.Task) error {
		return t.Exec("", nil, "docker", "pull", req.Image)
	})
	d.app.Audit(r, "image.pull", req.Image)
	core.OK(w, map[string]string{"task": task.ID})
}

func (d *Docker) imagePrune(w http.ResponseWriter, r *http.Request) {
	// dangling=true removes only untagged layers, never a tagged image you might reuse.
	filter := url.QueryEscape(`{"dangling":["true"]}`)
	data, err := d.call(r.Context(), http.MethodPost, "/images/prune?filters="+filter, nil)
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	var res struct{ SpaceReclaimed int64 }
	_ = json.Unmarshal(data, &res)
	d.app.Audit(r, "image.prune", "")
	core.OK(w, map[string]int64{"reclaimed": res.SpaceReclaimed})
}

func (d *Docker) imageRemove(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("id")
	if !validID.MatchString(name) {
		core.Fail(w, http.StatusBadRequest, "invalid image")
		return
	}
	if _, err := d.call(r.Context(), http.MethodDelete, "/images/"+name, nil); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	d.app.Audit(r, "image.remove", name)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Docker) networkCreate(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name string }
	if !core.Read(w, r, &req) {
		return
	}
	if !validID.MatchString(req.Name) {
		core.Fail(w, http.StatusBadRequest, "invalid network name")
		return
	}
	body, _ := json.Marshal(map[string]string{"Name": req.Name, "Driver": "bridge"})
	if _, err := d.call(r.Context(), http.MethodPost, "/networks/create", strings.NewReader(string(body))); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	d.app.Audit(r, "network.create", req.Name)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Docker) networkRemove(w http.ResponseWriter, r *http.Request) {
	nid, ok := id(w, r)
	if !ok {
		return
	}
	if _, err := d.call(r.Context(), http.MethodDelete, "/networks/"+nid, nil); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	d.app.Audit(r, "network.remove", nid)
	core.OK(w, map[string]bool{"ok": true})
}

func (d *Docker) volumeRemove(w http.ResponseWriter, r *http.Request) {
	vid, ok := id(w, r)
	if !ok {
		return
	}
	if _, err := d.call(r.Context(), http.MethodDelete, "/volumes/"+vid, nil); err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	d.app.Audit(r, "volume.remove", vid)
	core.OK(w, map[string]bool{"ok": true})
}

func readDaemon() (map[string]any, error) {
	cfg := map[string]any{}
	data, err := os.ReadFile(daemonConfig)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", daemonConfig, err)
	}
	return cfg, nil
}

func (d *Docker) mirrorsGet(w http.ResponseWriter, r *http.Request) {
	cfg, err := readDaemon()
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	mirrors := cfg["registry-mirrors"]
	if mirrors == nil {
		mirrors = []string{}
	}
	core.OK(w, map[string]any{"mirrors": mirrors})
}

// mirrorsSet edits daemon.json and reloads the daemon. registry-mirrors is one of the
// settings dockerd applies on SIGHUP, so no container restarts are involved.
func (d *Docker) mirrorsSet(w http.ResponseWriter, r *http.Request) {
	var req struct{ Mirrors []string }
	if !core.Read(w, r, &req) {
		return
	}
	for _, m := range req.Mirrors {
		if !validReg.MatchString(m) {
			core.Fail(w, http.StatusBadRequest, "invalid mirror URL: %s", m)
			return
		}
	}
	cfg, err := readDaemon()
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if len(req.Mirrors) == 0 {
		delete(cfg, "registry-mirrors")
	} else {
		cfg["registry-mirrors"] = req.Mirrors
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	restore, err := d.app.WriteConfig(daemonConfig, append(data, '\n'), 0o644)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if out, err := core.Run(r.Context(), "systemctl", "reload", "docker"); err != nil {
		_ = restore()
		core.Fail(w, http.StatusBadGateway, "docker reload failed, change reverted: %s", out)
		return
	}
	d.app.Audit(r, "docker.mirrors", strings.Join(req.Mirrors, ","))
	core.OK(w, map[string]bool{"ok": true})
}
