// Package cron manages scheduled jobs as /etc/cron.d/hypanel-<id> files. cron ignores
// files with a dot in their name, so a job is disabled by adding a .disabled suffix.
package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
)

const (
	dir      = "/etc/cron.d"
	prefix   = "hypanel-"
	disabled = ".disabled"
)

var (
	validID   = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)
	validSpec = regexp.MustCompile(`^(@(reboot|hourly|daily|weekly|monthly|yearly)|([0-9*/,-]+\s+){4}[0-9A-Za-z*/,-]+)$`)
)

type Cron struct{ app *core.App }

func New(app *core.App) *Cron { return &Cron{app: app} }

func (c *Cron) Name() string { return "cron" }

func (c *Cron) Routes(r chi.Router) {
	r.Get("/cron", c.list)
	r.Post("/cron", c.save)
	r.Post("/cron/{id}/run", c.run)
	r.Post("/cron/{id}/{action:enable|disable}", c.toggle)
	r.Delete("/cron/{id}", c.remove)
}

type Job struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Spec    string `json:"spec"`
	Command string `json:"command"`
	Enabled bool   `json:"enabled"`
}

// format writes a cron.d file. % starts a new line in cron, so it must be escaped.
func format(j Job) []byte {
	return []byte(fmt.Sprintf("# name: %s\nPATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n%s root %s\n",
		j.Name, j.Spec, strings.ReplaceAll(j.Command, "%", `\%`)))
}

func parse(id string, data string) (Job, bool) {
	j := Job{ID: id}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "# name:"):
			j.Name = strings.TrimSpace(strings.TrimPrefix(line, "# name:"))
		case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, "PATH="), strings.HasPrefix(line, "SHELL="), strings.HasPrefix(line, "MAILTO="):
		default:
			var fields []string
			if strings.HasPrefix(line, "@") {
				fields = strings.SplitN(line, " ", 3)
				if len(fields) < 3 || fields[1] != "root" {
					return j, false
				}
				j.Spec, j.Command = fields[0], fields[2]
			} else {
				fields = strings.Fields(line)
				if len(fields) < 7 || fields[5] != "root" {
					return j, false
				}
				j.Spec = strings.Join(fields[:5], " ")
				// Re-split so the command keeps its internal spacing.
				idx := strings.Index(line, " root ")
				j.Command = strings.TrimSpace(line[idx+len(" root "):])
			}
			j.Command = strings.ReplaceAll(j.Command, `\%`, "%")
			return j, true
		}
	}
	return j, false
}

func jobPath(id string) (string, bool, error) {
	if !validID.MatchString(id) {
		return "", false, fmt.Errorf("invalid job id")
	}
	on := filepath.Join(dir, prefix+id)
	if _, err := os.Stat(on); err == nil {
		return on, true, nil
	}
	if _, err := os.Stat(on + disabled); err == nil {
		return on + disabled, false, nil
	}
	return "", false, fmt.Errorf("job not found")
}

func (c *Cron) list(w http.ResponseWriter, r *http.Request) {
	matches, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
	out := []Job{}
	for _, f := range matches {
		base := filepath.Base(f)
		enabled := !strings.HasSuffix(base, disabled)
		id := strings.TrimSuffix(strings.TrimPrefix(base, prefix), disabled)
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if j, ok := parse(id, string(data)); ok {
			j.Enabled = enabled
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Name < out[k].Name })
	core.OK(w, out)
}

// save creates a job, or updates one when an ID is given.
func (c *Cron) save(w http.ResponseWriter, r *http.Request) {
	var j Job
	if !core.Read(w, r, &j) {
		return
	}
	j.Spec = strings.Join(strings.Fields(j.Spec), " ")
	j.Name = strings.TrimSpace(j.Name)
	j.Command = strings.TrimSpace(j.Command)
	if !validSpec.MatchString(j.Spec) {
		core.Fail(w, http.StatusBadRequest, "invalid schedule, expected five fields like: 30 3 * * *")
		return
	}
	if j.Name == "" || strings.ContainsAny(j.Name, "\r\n") {
		core.Fail(w, http.StatusBadRequest, "enter a single-line name")
		return
	}
	if j.Command == "" || strings.ContainsAny(j.Command, "\r\n") {
		core.Fail(w, http.StatusBadRequest, "the command must be a single line; put longer jobs in a script")
		return
	}
	path := ""
	if j.ID != "" {
		p, _, err := jobPath(j.ID)
		if err != nil {
			core.Fail(w, http.StatusNotFound, "%v", err)
			return
		}
		path = p
	} else {
		raw := make([]byte, 6)
		if _, err := rand.Read(raw); err != nil {
			core.Fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		j.ID = hex.EncodeToString(raw)
		path = filepath.Join(dir, prefix+j.ID)
	}
	if _, err := c.app.WriteConfig(path, format(j), 0o644); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	c.app.Audit(r, "cron.save", j.Name)
	core.OK(w, map[string]string{"id": j.ID})
}

func (c *Cron) toggle(w http.ResponseWriter, r *http.Request) {
	id, action := chi.URLParam(r, "id"), chi.URLParam(r, "action")
	path, enabled, err := jobPath(id)
	if err != nil {
		core.Fail(w, http.StatusNotFound, "%v", err)
		return
	}
	if enabled == (action == "enable") {
		core.OK(w, map[string]bool{"ok": true})
		return
	}
	next := filepath.Join(dir, prefix+id)
	if action == "disable" {
		next += disabled
	}
	if err := os.Rename(path, next); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	c.app.Audit(r, "cron."+action, id)
	core.OK(w, map[string]bool{"ok": true})
}

func (c *Cron) remove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path, _, err := jobPath(id)
	if err != nil {
		core.Fail(w, http.StatusNotFound, "%v", err)
		return
	}
	if err := os.Remove(path); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	c.app.Audit(r, "cron.delete", id)
	core.OK(w, map[string]bool{"ok": true})
}

// run starts the job now and shows its output, which is how you check a new schedule.
func (c *Cron) run(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	path, _, err := jobPath(id)
	if err != nil {
		core.Fail(w, http.StatusNotFound, "%v", err)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	j, ok := parse(id, string(data))
	if !ok {
		core.Fail(w, http.StatusBadRequest, "couldn't read this job file")
		return
	}
	task := c.app.Tasks.Start("Run "+j.Name, func(_ context.Context, t *core.Task) error {
		return t.Exec("/", nil, "/bin/sh", "-c", j.Command)
	})
	c.app.Audit(r, "cron.run", j.Name)
	core.OK(w, map[string]string{"task": task.ID})
}
