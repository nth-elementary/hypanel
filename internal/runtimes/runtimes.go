// Package runtimes creates ready-to-run environments for PHP, Node, Python and MariaDB.
// Each one is an ordinary compose stack in /opt/stacks, so after creation it can be
// managed, edited or removed like any other stack, and works without the panel.
package runtimes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
	"hypanel/internal/stacks"
)

var (
	validName    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)
	validVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)
	validExt     = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

type Runtimes struct{ app *core.App }

func New(app *core.App) *Runtimes { return &Runtimes{app: app} }

func (s *Runtimes) Name() string { return "runtimes" }

func (s *Runtimes) Routes(r chi.Router) {
	r.Post("/runtimes", s.create)
	r.Get("/runtimes/catalog", func(w http.ResponseWriter, _ *http.Request) {
		core.OK(w, map[string]any{
			"php":     map[string]any{"versions": []string{"8.5", "8.4", "8.3"}, "port": 9000},
			"node":    map[string]any{"versions": []string{"24", "22"}, "port": 3000},
			"python":  map[string]any{"versions": []string{"3.14", "3.13", "3.12"}, "port": 8000},
			"mariadb": map[string]any{"versions": []string{"11.8", "11.4", "10.11"}, "port": 3307},
		})
	})
}

type Request struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	Port       int      `json:"port"`
	Code       string   `json:"code"`       // Node/Python: source directory on the host
	Command    string   `json:"command"`    // Node/Python: start command
	AppPort    int      `json:"appPort"`    // Node/Python: port the app listens on
	Extensions []string `json:"extensions"` // PHP
}

// Network is the external Docker network every generated stack joins, so apps can reach a
// database or cache by container name. deploy/install.sh creates it.
var Network = "hypanel-network"

// The daemon has no default log limit, so every generated stack sets its own.
const logging = `    logging:
      driver: json-file
      options:
        max-size: 50m
        max-file: "3"
`

var templates = map[string]*template.Template{
	"php": template.Must(template.New("php").Parse(`networks:
  {{.Network}}:
    external: true
services:
  php:
    build: .
    image: hypanel/{{.Name}}:{{.Version}}
    container_name: {{.Name}}
    restart: unless-stopped
    networks:
      - {{.Network}}
    ports:
      - 127.0.0.1:{{.Port}}:9000
    volumes:
      - /www:/www
` + logging)),
	"node": template.Must(template.New("node").Parse(`networks:
  {{.Network}}:
    external: true
services:
  app:
    image: node:{{.Version}}-bookworm-slim
    container_name: {{.Name}}
    restart: unless-stopped
    working_dir: /app
    command: {{.Quoted}}
    networks:
      - {{.Network}}
    ports:
      - 127.0.0.1:{{.Port}}:{{.AppPort}}
    volumes:
      - {{.Code}}:/app
` + logging)),
	"python": template.Must(template.New("python").Parse(`networks:
  {{.Network}}:
    external: true
services:
  app:
    image: python:{{.Version}}-slim
    container_name: {{.Name}}
    restart: unless-stopped
    working_dir: /app
    command: {{.Quoted}}
    networks:
      - {{.Network}}
    ports:
      - 127.0.0.1:{{.Port}}:{{.AppPort}}
    volumes:
      - {{.Code}}:/app
` + logging)),
	"mariadb": template.Must(template.New("mariadb").Parse(`networks:
  {{.Network}}:
    external: true
services:
  db:
    image: mariadb:{{.Version}}
    container_name: {{.Name}}
    restart: unless-stopped
    environment:
      MARIADB_ROOT_PASSWORD: ${MARIADB_ROOT_PASSWORD}
    networks:
      - {{.Network}}
    ports:
      - 127.0.0.1:{{.Port}}:3306
    volumes:
      - ./data:/var/lib/mysql
` + logging)),
}

const phpDockerfile = `FROM php:{{.Version}}-fpm-alpine
ADD --chmod=755 https://github.com/mlocati/docker-php-extension-installer/releases/latest/download/install-php-extensions /usr/local/bin/
RUN install-php-extensions {{.Extensions}}
`

// defaultPHPExtensions cover what WordPress, MediaWiki and most frameworks expect.
var defaultPHPExtensions = []string{"mysqli", "pdo_mysql", "gd", "intl", "zip", "exif", "opcache", "bcmath", "apcu", "memcached", "redis"}

// Render validates a request and returns the files to write for the stack.
func Render(req Request) (map[string][]byte, error) {
	tmpl, ok := templates[req.Kind]
	if !ok {
		return nil, fmt.Errorf("kind must be php, node, python or mariadb")
	}
	if !validName.MatchString(req.Name) {
		return nil, fmt.Errorf("name may only contain letters, digits, - and _")
	}
	if !validVersion.MatchString(req.Version) {
		return nil, fmt.Errorf("invalid version %q", req.Version)
	}
	if req.Port < 1024 || req.Port > 65535 {
		return nil, fmt.Errorf("port must be between 1024 and 65535")
	}
	data := map[string]any{"Name": req.Name, "Version": req.Version, "Port": req.Port, "AppPort": req.AppPort, "Code": req.Code, "Network": Network}
	files := map[string][]byte{}

	switch req.Kind {
	case "node", "python":
		if !filepath.IsAbs(req.Code) || strings.ContainsAny(req.Code, " :'\"\n\t#") {
			return nil, fmt.Errorf("source directory must be an absolute path without spaces")
		}
		if req.AppPort < 1 || req.AppPort > 65535 {
			return nil, fmt.Errorf("invalid app port")
		}
		if strings.TrimSpace(req.Command) == "" || strings.ContainsAny(req.Command, "\n\r") {
			return nil, fmt.Errorf("enter a start command")
		}
		// As a JSON-style array the command stays one YAML value, whatever it contains.
		data["Quoted"] = fmt.Sprintf(`["sh", "-c", %s]`, strconv.Quote(req.Command))
	case "php":
		exts := append([]string{}, defaultPHPExtensions...)
		for _, e := range req.Extensions {
			if !validExt.MatchString(e) {
				return nil, fmt.Errorf("invalid extension %q", e)
			}
			exts = append(exts, e)
		}
		var buf bytes.Buffer
		t := template.Must(template.New("Dockerfile").Parse(phpDockerfile))
		if err := t.Execute(&buf, map[string]string{"Version": req.Version, "Extensions": strings.Join(exts, " ")}); err != nil {
			return nil, err
		}
		files["Dockerfile"] = buf.Bytes()
	case "mariadb":
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		files[".env"] = []byte("MARIADB_ROOT_PASSWORD=" + hex.EncodeToString(raw) + "\n")
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	files["docker-compose.yml"] = buf.Bytes()
	return files, nil
}

func (s *Runtimes) create(w http.ResponseWriter, r *http.Request) {
	var req Request
	if !core.Read(w, r, &req) {
		return
	}
	files, err := Render(req)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	dir := filepath.Join(stacks.Root, req.Name)
	if _, err := os.Lstat(dir); err == nil {
		core.Fail(w, http.StatusConflict, "stack %s already exists", req.Name)
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	for name, data := range files {
		perm := os.FileMode(0o644)
		if name == ".env" {
			perm = 0o600
		}
		if err := core.AtomicWrite(filepath.Join(dir, name), data, perm); err != nil {
			_ = os.RemoveAll(dir)
			core.Fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
	}
	task := s.app.Tasks.Start(fmt.Sprintf("Create %s %s", req.Kind, req.Name), func(_ context.Context, t *core.Task) error {
		args := []string{"compose", "up", "-d"}
		if req.Kind == "php" {
			args = append(args, "--build")
		}
		return t.Exec(dir, nil, "docker", args...)
	})
	s.app.Audit(r, "runtime.create", req.Kind+" "+req.Name)
	core.OK(w, map[string]string{"task": task.ID})
}
