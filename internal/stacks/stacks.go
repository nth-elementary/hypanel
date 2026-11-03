// Package stacks manages docker compose projects, each a directory in /opt/stacks.
// The files on disk are the only state: anything edited by hand shows up here too.
package stacks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
)

const Root = "/opt/stacks"

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// composeFiles are the files the editor exposes, in the order compose reads them.
var composeFiles = []string{"docker-compose.yml", "docker-compose.override.yml", ".env"}

type Stacks struct{ app *core.App }

func New(app *core.App) *Stacks { return &Stacks{app: app} }

func (s *Stacks) Name() string { return "stacks" }

func (s *Stacks) Routes(r chi.Router) {
	r.Get("/stacks", s.list)
	r.Post("/stacks", s.create)
	r.Get("/stacks/{name}/files", s.files)
	r.Post("/stacks/{name}/files", s.saveFile)
	r.Get("/stacks/{name}/logs", s.logs)
	r.Post("/stacks/{name}/{action:up|down|restart|pull}", s.action)
}

type Container struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Status string `json:"status"`
	Image  string `json:"image"`
	Ports  string `json:"ports"`
}

type Stack struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	Containers []Container `json:"containers"`
	Status     string      `json:"status"` // running, partial, stopped
}

// projectDir resolves a stack name, following symlinks so that a linked stack
// (one whose files live elsewhere) works the same as a directory in /opt/stacks.
func projectDir(name string) (string, error) {
	if !validName.MatchString(name) || strings.HasPrefix(name, "_") {
		return "", fmt.Errorf("invalid stack name")
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(Root, name))
	if err != nil {
		return "", fmt.Errorf("stack %s not found", name)
	}
	return dir, nil
}

func (s *Stacks) list(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(Root)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	byDir := containersByWorkdir(r.Context())
	out := []Stack{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		dir, err := projectDir(name)
		if err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "docker-compose.yml")); err != nil {
			continue
		}
		st := Stack{Name: name, Path: dir, Containers: byDir[dir], Status: "stopped"}
		if st.Containers == nil {
			st.Containers = []Container{}
		}
		running := 0
		for _, c := range st.Containers {
			if c.State == "running" {
				running++
			}
		}
		switch {
		case running > 0 && running == len(st.Containers):
			st.Status = "running"
		case running > 0:
			st.Status = "partial"
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	core.OK(w, out)
}

// containersByWorkdir groups containers by the compose working directory label.
func containersByWorkdir(ctx context.Context) map[string][]Container {
	out := map[string][]Container{}
	raw, err := core.Run(ctx, "docker", "ps", "-a", "--format", "{{json .}}")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var c struct{ Names, State, Status, Image, Ports, Labels string }
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		for _, label := range strings.Split(c.Labels, ",") {
			if v, ok := strings.CutPrefix(label, "com.docker.compose.project.working_dir="); ok {
				if real, err := filepath.EvalSymlinks(v); err == nil {
					v = real
				}
				out[v] = append(out[v], Container{c.Names, c.State, c.Status, c.Image, c.Ports})
			}
		}
	}
	return out
}

func (s *Stacks) create(w http.ResponseWriter, r *http.Request) {
	var req struct{ Name, Compose string }
	if !core.Read(w, r, &req) {
		return
	}
	if !validName.MatchString(req.Name) {
		core.Fail(w, http.StatusBadRequest, "name may only contain letters, digits, - and _")
		return
	}
	dir := filepath.Join(Root, req.Name)
	if _, err := os.Lstat(dir); err == nil {
		core.Fail(w, http.StatusConflict, "stack %s already exists", req.Name)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	file := filepath.Join(dir, "docker-compose.yml")
	if err := core.AtomicWrite(file, []byte(req.Compose), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if out, err := core.RunIn(r.Context(), dir, nil, "docker", "compose", "config", "-q"); err != nil {
		_ = os.RemoveAll(dir)
		core.Fail(w, http.StatusBadRequest, "invalid compose file: %s", strings.TrimSpace(out))
		return
	}
	s.app.Audit(r, "stack.create", req.Name)
	core.OK(w, map[string]bool{"ok": true})
}

func (s *Stacks) files(w http.ResponseWriter, r *http.Request) {
	dir, err := projectDir(chi.URLParam(r, "name"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	out := map[string]string{}
	for _, f := range composeFiles {
		if data, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			out[f] = string(data)
		}
	}
	core.OK(w, map[string]any{"path": dir, "files": out})
}

// saveFile validates the merged project and restores the old file if it doesn't parse.
func (s *Stacks) saveFile(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	dir, err := projectDir(name)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	var req struct{ File, Content string }
	if !core.Read(w, r, &req) {
		return
	}
	allowed := false
	for _, f := range composeFiles {
		allowed = allowed || f == req.File
	}
	if !allowed {
		core.Fail(w, http.StatusBadRequest, "not a compose file: %s", req.File)
		return
	}
	perm := os.FileMode(0o644)
	if req.File == ".env" {
		perm = 0o600 // env files usually hold passwords
	}
	restore, err := s.app.WriteConfig(filepath.Join(dir, req.File), []byte(req.Content), perm)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if out, err := core.RunIn(r.Context(), dir, nil, "docker", "compose", "config", "-q"); err != nil {
		_ = restore()
		core.Fail(w, http.StatusBadRequest, "not saved, compose rejected it: %s", strings.TrimSpace(out))
		return
	}
	s.app.Audit(r, "stack.edit", name+"/"+req.File)
	core.OK(w, map[string]bool{"ok": true})
}

func (s *Stacks) logs(w http.ResponseWriter, r *http.Request) {
	dir, err := projectDir(chi.URLParam(r, "name"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	tail := "500"
	if n, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && n > 0 && n <= 10000 {
		tail = strconv.Itoa(n)
	}
	out, err := core.RunIn(r.Context(), dir, nil, "docker", "compose", "logs", "--no-color", "--tail", tail, "--timestamps")
	if err != nil {
		core.Fail(w, http.StatusBadGateway, "%v", err)
		return
	}
	core.OK(w, map[string]string{"logs": out})
}

func (s *Stacks) action(w http.ResponseWriter, r *http.Request) {
	name, action := chi.URLParam(r, "name"), chi.URLParam(r, "action")
	dir, err := projectDir(name)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	args := map[string][]string{
		"up":      {"compose", "up", "-d", "--remove-orphans"},
		"down":    {"compose", "down"},
		"restart": {"compose", "restart"},
		"pull":    {"compose", "pull"},
	}[action]
	task := s.app.Tasks.Start(fmt.Sprintf("%s %s", action, name), func(_ context.Context, t *core.Task) error {
		return t.Exec(dir, nil, "docker", args...)
	})
	s.app.Audit(r, "stack."+action, name)
	core.OK(w, map[string]string{"task": task.ID})
}
