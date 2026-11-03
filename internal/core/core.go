// Package core holds what every module shares: the app handle, JSON helpers,
// command execution, atomic config writes and background tasks.
package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/go-chi/chi/v5"
	_ "modernc.org/sqlite"
)

// Module is one panel feature. Adding a feature means adding one package that
// implements this, registering it in main.go, and adding one UI page.
type Module interface {
	Name() string
	Routes(r chi.Router)
}

type App struct {
	DB      *sql.DB
	DataDir string
	Tasks   *Tasks
}

func Open(dataDir string) (*App, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "backups"), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.Join(dataDir, "panel.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS settings (k TEXT PRIMARY KEY, v TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS audit (ts INTEGER NOT NULL, ip TEXT NOT NULL, action TEXT NOT NULL, detail TEXT NOT NULL);
		CREATE INDEX IF NOT EXISTS audit_ts ON audit(ts);`)
	if err != nil {
		return nil, err
	}
	return &App{DB: db, DataDir: dataDir, Tasks: NewTasks()}, nil
}

func (a *App) Setting(key string) string {
	var v string
	_ = a.DB.QueryRow(`SELECT v FROM settings WHERE k=?`, key).Scan(&v)
	return v
}

func (a *App) SetSetting(key, value string) error {
	_, err := a.DB.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, key, value)
	return err
}

// Audit records a state-changing action. Failing to record must not fail the action.
func (a *App) Audit(r *http.Request, action, detail string) {
	_, _ = a.DB.Exec(`INSERT INTO audit(ts,ip,action,detail) VALUES(?,?,?,?)`, time.Now().Unix(), ClientIP(r), action, detail)
}

// ClientIP trusts X-Real-IP because the panel only listens on loopback behind nginx.
func ClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func OK(w http.ResponseWriter, v any) { JSON(w, http.StatusOK, v) }

func Fail(w http.ResponseWriter, status int, format string, args ...any) {
	JSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

// FailInUse replies 409 listing what depends on a path, so the UI can ask before forcing.
func FailInUse(w http.ResponseWriter, path string, users []string) {
	JSON(w, http.StatusConflict, map[string]any{"error": path + " is in use", "inUse": users})
}

// Read decodes a JSON body, replying 400 itself when it returns false.
func Read(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20))
	if err := dec.Decode(v); err != nil {
		Fail(w, http.StatusBadRequest, "invalid request: %v", err)
		return false
	}
	return true
}

// Run executes a command and returns its combined output.
func Run(ctx context.Context, name string, args ...string) (string, error) {
	return RunIn(ctx, "", nil, name, args...)
}

func RunIn(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil && out.Len() > 0 {
		return out.String(), errors.New(string(bytes.TrimSpace(out.Bytes())))
	}
	return out.String(), err
}

// WriteConfig atomically replaces path and keeps a timestamped copy of what it
// replaced. It returns a function that restores the previous state.
func (a *App) WriteConfig(path string, data []byte, perm os.FileMode) (restore func() error, err error) {
	old, readErr := os.ReadFile(path)
	existed := readErr == nil
	if existed {
		name := fmt.Sprintf("%s.%d", filepath.Base(path), time.Now().UnixNano())
		if err := os.WriteFile(filepath.Join(a.DataDir, "backups", name), old, 0o600); err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		if info, err := os.Stat(path); err == nil {
			perm = info.Mode().Perm()
		}
	}
	if err := AtomicWrite(path, data, perm); err != nil {
		return nil, err
	}
	return func() error {
		if existed {
			return AtomicWrite(path, old, perm)
		}
		return os.Remove(path)
	}, nil
}

func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hypanel-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
