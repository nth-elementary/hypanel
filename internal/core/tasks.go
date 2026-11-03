package core

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// Tasks runs long operations (image pulls, certificate issuance, compose up) in the
// background, so they outlive the request and the UI can follow their output.
type Tasks struct {
	mu    sync.Mutex
	items map[string]*Task
}

type Task struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished"`
	Error    string `json:"error"`
	Log      string `json:"log"`

	mu sync.Mutex
}

const maxTaskLog = 256 << 10

func NewTasks() *Tasks { return &Tasks{items: map[string]*Task{}} }

func (t *Task) Printf(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Log += fmt.Sprintf(format, args...)
	if len(t.Log) > maxTaskLog {
		t.Log = t.Log[len(t.Log)-maxTaskLog:]
	}
}

func (t *Task) snapshot() Task {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Task{ID: t.ID, Title: t.Title, Started: t.Started, Finished: t.Finished, Error: t.Error, Log: t.Log}
}

// Exec runs a command, streaming its output into the task log.
func (t *Task) Exec(dir string, env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		defer close(done)
		s := bufio.NewScanner(pr)
		s.Buffer(make([]byte, 64<<10), 1<<20)
		for s.Scan() {
			t.Printf("%s\n", s.Text())
		}
	}()
	t.Printf("$ %s %v\n", name, args)
	err := cmd.Run()
	pw.Close()
	<-done
	return err
}

// Start launches fn in the background and returns immediately.
func (ts *Tasks) Start(title string, fn func(ctx context.Context, t *Task) error) *Task {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	t := &Task{ID: hex.EncodeToString(b), Title: title, Started: time.Now().Unix()}
	ts.mu.Lock()
	ts.items[t.ID] = t
	ts.prune()
	ts.mu.Unlock()
	go func() {
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			return fn(context.Background(), t)
		}()
		t.mu.Lock()
		t.Finished = time.Now().Unix()
		if err != nil {
			t.Error = err.Error()
		}
		t.mu.Unlock()
	}()
	return t
}

// prune keeps the most recent finished tasks. Callers hold ts.mu.
func (ts *Tasks) prune() {
	if len(ts.items) <= 100 {
		return
	}
	cutoff := time.Now().Add(-time.Hour).Unix()
	for id, t := range ts.items {
		s := t.snapshot()
		if s.Finished != 0 && s.Finished < cutoff {
			delete(ts.items, id)
		}
	}
}

func (ts *Tasks) Routes(r chi.Router) {
	r.Get("/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		t := ts.items[chi.URLParam(r, "id")]
		ts.mu.Unlock()
		if t == nil {
			Fail(w, http.StatusNotFound, "task not found")
			return
		}
		s := t.snapshot()
		OK(w, &s)
	})
}
