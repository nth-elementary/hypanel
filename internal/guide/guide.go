// Package guide serves two documents: the Guide, an editable note kept on the server that
// starts as the panel's README, and the fixed guide for coding agents. deploy/install.sh
// also places the agent guide where agents working on the server will find it.
package guide

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	readme "hypanel"
	"hypanel/internal/core"
)

//go:embed AGENTS.md
var agents string

type Guide struct {
	app  *core.App
	path string
}

func New(app *core.App) *Guide { return &Guide{app: app, path: filepath.Join(app.DataDir, "guide.md")} }

func (g *Guide) Name() string { return "guide" }

func (g *Guide) Routes(r chi.Router) {
	r.Get("/guide", g.get)
	r.Post("/guide", g.save)
	r.Post("/guide/prefs", g.savePrefs)
	r.Get("/guide/agents", func(w http.ResponseWriter, _ *http.Request) {
		core.OK(w, map[string]string{"text": agents})
	})
}

// load returns the note, writing the README on first use. After that the file belongs to
// the user: panel upgrades never replace it.
func (g *Guide) load() (string, int64, error) {
	if _, err := os.Stat(g.path); os.IsNotExist(err) {
		if err := core.AtomicWrite(g.path, []byte(readme.Readme), 0o600); err != nil {
			return "", 0, err
		}
	}
	data, err := os.ReadFile(g.path)
	if err != nil {
		return "", 0, err
	}
	info, err := os.Stat(g.path)
	if err != nil {
		return "", 0, err
	}
	// Milliseconds, because nanoseconds don't survive a JavaScript number: the browser would
	// send back a rounded value and every save would look like a conflict.
	return string(data), info.ModTime().UnixMilli(), nil
}

func (g *Guide) prefs() (menu, dashboard bool) {
	return g.app.Setting("guide_menu") != "off", g.app.Setting("guide_dashboard") != "off"
}

func (g *Guide) get(w http.ResponseWriter, _ *http.Request) {
	text, mod, err := g.load()
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	menu, dashboard := g.prefs()
	core.OK(w, map[string]any{"text": text, "modTime": mod, "showMenu": menu, "showDashboard": dashboard, "path": g.path})
}

// save refuses to overwrite a version changed elsewhere since the editor opened it. The
// replaced text goes to the panel's backups, so an edit can always be undone by hand.
func (g *Guide) save(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text    string
		ModTime int64
	}
	if !core.Read(w, r, &req) {
		return
	}
	_, mod, err := g.load()
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if req.ModTime != 0 && req.ModTime != mod {
		core.Fail(w, http.StatusConflict, "the guide changed since you opened it; reload to get the latest")
		return
	}
	if _, err := g.app.WriteConfig(g.path, []byte(req.Text), 0o600); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	g.app.Audit(r, "guide.edit", "")
	_, mod, err = g.load()
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	core.OK(w, map[string]int64{"modTime": mod})
}

func (g *Guide) savePrefs(w http.ResponseWriter, r *http.Request) {
	var req struct{ ShowMenu, ShowDashboard bool }
	if !core.Read(w, r, &req) {
		return
	}
	flag := func(on bool) string {
		if on {
			return "on"
		}
		return "off"
	}
	if err := g.app.SetSetting("guide_menu", flag(req.ShowMenu)); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := g.app.SetSetting("guide_dashboard", flag(req.ShowDashboard)); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	core.OK(w, map[string]bool{"ok": true})
}
