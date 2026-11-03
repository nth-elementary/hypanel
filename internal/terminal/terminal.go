// Package terminal bridges a WebSocket to a shell in a PTY, either on the host or
// inside a container. Binary frames carry terminal bytes; text frames carry resizes.
package terminal

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"

	"hypanel/internal/auth"
	"hypanel/internal/core"
)

var containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

type Terminal struct {
	app      *core.App
	upgrader websocket.Upgrader
}

func New(app *core.App) *Terminal {
	return &Terminal{
		app:      app,
		upgrader: websocket.Upgrader{CheckOrigin: auth.SameOrigin},
	}
}

func (t *Terminal) Name() string { return "terminal" }

func (t *Terminal) Routes(r chi.Router) {
	r.Get("/terminal/ws", t.serve)
}

func (t *Terminal) serve(w http.ResponseWriter, r *http.Request) {
	var cmd *exec.Cmd
	target := "host"
	if c := r.URL.Query().Get("container"); c != "" {
		if !containerName.MatchString(c) {
			core.Fail(w, http.StatusBadRequest, "invalid container name")
			return
		}
		target = "container " + c
		// bash if the image has it, otherwise sh (alpine images).
		cmd = exec.Command("docker", "exec", "-it", c, "sh", "-c", "command -v bash >/dev/null && exec bash || exec sh")
	} else {
		cmd = exec.Command("/bin/bash", "-l")
		cmd.Dir = "/root"
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "HOME=/root")

	conn, err := t.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade has already replied
	}
	defer conn.Close()

	t.app.Audit(r, "terminal.open", target)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("failed to start shell: "+err.Error()))
		return
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = ptmx.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
	}
	defer stop()
	defer func() { _ = cmd.Wait() }()

	// Shell to browser. A closed PTY ends the session.
	go func() {
		defer conn.Close()
		buf := make([]byte, 16<<10)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Browser to shell. Pings keep idle sessions alive through nginx's proxy timeout.
	pingStop := make(chan struct{})
	defer close(pingStop)
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-tick.C:
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
		}
	}()
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch kind {
		case websocket.BinaryMessage:
			if _, err := ptmx.Write(data); err != nil {
				return
			}
		case websocket.TextMessage:
			var size struct{ Cols, Rows uint16 }
			if json.Unmarshal(data, &size) == nil && size.Cols > 0 && size.Rows > 0 {
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
			}
		}
	}
}
