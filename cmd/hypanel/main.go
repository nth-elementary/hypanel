// HyPanel is a lightweight server control panel: one binary, plain config files, and
// nothing in the path of live traffic. If it stops, sites and containers keep running.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"hypanel/internal/auth"
	"hypanel/internal/core"
	"hypanel/internal/cron"
	"hypanel/internal/database"
	"hypanel/internal/docker"
	"hypanel/internal/files"
	"hypanel/internal/firewall"
	"hypanel/internal/guide"
	"hypanel/internal/monitor"
	"hypanel/internal/runtimes"
	"hypanel/internal/ssl"
	"hypanel/internal/stacks"
	"hypanel/internal/terminal"
	"hypanel/internal/websites"
	"hypanel/web"
)

// Optional features add themselves here from an init function in their own file in this
// package, so a build without those files is still complete.
var (
	extraModules  []func(*core.App) core.Module
	extraCommands = map[string]func(*core.App) error{}
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7777", "address to listen on")
	dataDir := flag.String("data", "/var/lib/hypanel", "data directory")
	flag.Parse()

	if err := run(*listen, *dataDir, flag.Args()); err != nil {
		log.Fatal(err)
	}
}

func run(listen, dataDir string, args []string) error {
	app, err := core.Open(dataDir)
	if err != nil {
		return err
	}
	a, err := auth.New(app)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		if args[0] == "admin" && len(args) == 2 {
			return setAdmin(a, args[1])
		}
		if command, ok := extraCommands[args[0]]; ok && len(args) == 1 {
			return command(app)
		}
		return fmt.Errorf("usage: hypanel [flags] | hypanel admin <username>")
	}

	mon, err := monitor.New(app)
	if err != nil {
		return err
	}
	certs, err := ssl.New(app)
	if err != nil {
		return err
	}

	// To add a feature: write a package implementing core.Module and list it here.
	modules := []core.Module{
		mon,
		terminal.New(app),
		files.New(app),
		firewall.New(app),
		docker.New(app),
		stacks.New(app),
		websites.New(app),
		certs,
		database.New(app),
		runtimes.New(app),
		cron.New(app),
		guide.New(app),
	}
	for _, module := range extraModules {
		modules = append(modules, module(app))
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)
	r.Route("/api", func(r chi.Router) {
		a.Routes(r)
		r.Group(func(r chi.Router) {
			r.Use(a.Require)
			a.ProtectedRoutes(r)
			app.Tasks.Routes(r)
			for _, m := range modules {
				m.Routes(r)
			}
		})
		r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
			core.Fail(w, http.StatusNotFound, "not found")
		})
	})
	r.Handle("/*", spa(web.Dist()))

	// No write timeout: uploads, downloads and terminals are legitimately long.
	srv := &http.Server{Addr: listen, Handler: r, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go mon.Run(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("hypanel listening on %s", listen)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

// setAdmin generates a password, since typing one into a shell would leave it in history.
func setAdmin(a *auth.Auth, user string) error {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	otp, err := a.SetAdmin(user, password)
	if err != nil {
		return err
	}
	fmt.Printf("Username: %s\nPassword: %s\n2FA URL:  %s\n\nAdd the 2FA URL to an authenticator app. Change the password after logging in.\n", user, password, otp)
	return nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		// CodeMirror and xterm inject styles at runtime, so style-src needs unsafe-inline.
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' wss: ws:; frame-ancestors 'none'; base-uri 'none'")
		next.ServeHTTP(w, r)
	})
}

// spa serves built assets, and index.html for client-side routes.
func spa(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" {
			if _, err := fs.Stat(dist, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		data, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "UI not built", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
}

