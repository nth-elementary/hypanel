// Package websites manages nginx vhosts in /etc/nginx/sites. Configs stay plain files,
// so hand edits and migrated sites work alongside generated ones. Every change passes
// `nginx -t` before a reload, and is rolled back if the test fails.
package websites

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/cfdns"
	"hypanel/internal/core"
)

const (
	ConfDir  = "/etc/nginx/sites"
	SitesDir = "/www/sites"
	// SSLDir is outside /www because /www is mounted into PHP containers, and a
	// compromised site must not be able to read private keys.
	SSLDir = "/etc/nginx/ssl"
	disabled = ".disabled"
)

var (
	validSite   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	validDomain = regexp.MustCompile(`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)
	validProxy  = regexp.MustCompile(`^https?://[a-zA-Z0-9.:\[\]_-]+(/[a-zA-Z0-9._~/-]*)?$`)
	validFPM    = regexp.MustCompile(`^[a-zA-Z0-9.-]+:\d{1,5}$`)
	// The Host header: an nginx variable from this short list, or a literal host name.
	validProxyHost = regexp.MustCompile(`^(\$(host|http_host|proxy_host)|[a-zA-Z0-9]([a-zA-Z0-9.-]*[a-zA-Z0-9])?(:\d{1,5})?)$`)
)

type Websites struct{ app *core.App }

func New(app *core.App) *Websites { return &Websites{app: app} }

func (s *Websites) Name() string { return "websites" }

func (s *Websites) Routes(r chi.Router) {
	r.Get("/websites", s.list)
	r.Post("/websites", s.create)
	r.Get("/websites/{name}/conf", s.confGet)
	r.Post("/websites/{name}/conf", s.confSet)
	r.Post("/websites/{name}/{action:enable|disable}", s.toggle)
	r.Delete("/websites/{name}", s.remove)
	r.Get("/websites/{name}/log", s.log)
	r.Post("/websites/{name}/log/vacuum", s.logVacuum)
	r.Get("/websites/dns-coverage", s.dnsCoverage)
	r.Post("/websites/reload", s.reload)
}

type Site struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	Kind    string   `json:"kind"` // static, php, proxy
	Target  string   `json:"target"`
	HTTPS   bool     `json:"https"`
	Cert    string   `json:"cert"`
	Enabled bool     `json:"enabled"`
}

var (
	reServerName = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	reRoot       = regexp.MustCompile(`(?m)^\s*root\s+([^;]+);`)
	reFastCGI    = regexp.MustCompile(`(?m)^\s*fastcgi_pass\s+([^;]+);`)
	reProxy      = regexp.MustCompile(`(?m)^\s*proxy_pass\s+([^;]+);`)
	reCert       = regexp.MustCompile(`(?m)^\s*ssl_certificate\s+([^;]+);`)
	reInclude    = regexp.MustCompile(`(?m)^\s*include\s+(/www/sites/[^;]+);`)
)

// parseSite extracts a summary from a vhost. Configs may be hand-written, so anything
// it can't recognise simply leaves a field empty.
func parseSite(name, conf string) (Site, bool) {
	m := reServerName.FindStringSubmatch(conf)
	if m == nil {
		return Site{}, false // maps and other http-level snippets
	}
	site := Site{Name: name, Domains: strings.Fields(m[1]), Kind: "static"}
	if c := reCert.FindStringSubmatch(conf); c != nil {
		site.HTTPS, site.Cert = true, strings.TrimSpace(c[1])
	}
	if r := reRoot.FindStringSubmatch(serverLevel(conf)); r != nil {
		site.Target = strings.TrimSpace(r[1])
	}
	if f := reFastCGI.FindStringSubmatch(conf); f != nil {
		site.Kind, site.Target = "php", strings.TrimSpace(f[1])
		return site, true
	}
	if p := proxyTarget(conf); p != "" {
		site.Kind, site.Target = "proxy", p
	}
	return site, true
}

// serverLevel returns only the directives directly inside server blocks, without nested
// blocks or comments. Sites keep a separate root inside their acme-challenge location, which
// is what a plain search for "root" finds first.
func serverLevel(conf string) string {
	var b strings.Builder
	depth := 0
	for _, line := range strings.Split(conf, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, c := range line {
			switch c {
			case '{':
				depth++
			case '}':
				depth--
			default:
				if depth == 1 {
					b.WriteRune(c)
				}
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// proxyTarget finds proxy_pass in the vhost or in a site's proxy/*.conf include, a layout
// other panels use.
func proxyTarget(conf string) string {
	if p := reProxy.FindStringSubmatch(conf); p != nil {
		return strings.TrimSpace(p[1])
	}
	for _, inc := range reInclude.FindAllStringSubmatch(conf, -1) {
		matches, _ := filepath.Glob(strings.TrimSpace(inc[1]))
		for _, f := range matches {
			if data, err := os.ReadFile(f); err == nil {
				if p := reProxy.FindSubmatch(data); p != nil {
					return strings.TrimSpace(string(p[1]))
				}
			}
		}
	}
	return ""
}

func (s *Websites) list(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(ConfDir)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := []Site{}
	for _, e := range entries {
		file := e.Name()
		enabled := strings.HasSuffix(file, ".conf")
		base := strings.TrimSuffix(strings.TrimSuffix(file, disabled), ".conf")
		if !enabled && !strings.HasSuffix(file, ".conf"+disabled) {
			continue
		}
		if base == "00.default" || base == "default" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(ConfDir, file))
		if err != nil {
			continue
		}
		if site, ok := parseSite(base, string(data)); ok {
			site.Enabled = enabled
			out = append(out, site)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	core.OK(w, out)
}

type CreateRequest struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	Kind    string   `json:"kind"`
	Target  string   `json:"target"` // proxy URL, or PHP-FPM host:port
	Cert    string   `json:"cert"`   // optional path to fullchain.pem
	Key     string   `json:"key"`

	// Reverse proxy options.
	ProxyHost    string `json:"proxyHost"`    // Host header sent upstream; $host when empty
	Cache        bool   `json:"cache"`        // cache responses in the shared nginx zone
	CacheMinutes int    `json:"cacheMinutes"` // how long a 200/301/302 stays cached

	// Options for the create request only.
	AddDNS  bool `json:"addDns"`  // add a Cloudflare record when an account covers the domain
	Proxied bool `json:"proxied"` // whether that record goes through Cloudflare's proxy
}

var confTemplate = template.Must(template.New("site").Parse(`server {
    listen 80;
{{- if .HTTPS}}
    listen 443 ssl http2;
{{- end}}
    server_name {{.Domains}};

    access_log /www/sites/{{.Name}}/log/access.log;
    error_log /www/sites/{{.Name}}/log/error.log;

    location ^~ /.well-known/acme-challenge {
        allow all;
        root /usr/share/nginx/html;
    }
{{- if .HTTPS}}

    set $visitor_scheme "$scheme$http_x_forwarded_proto";
    if ($visitor_scheme ~ ^http(http)?$) {
        return 301 https://$host$request_uri;
    }

    ssl_certificate {{.Cert}};
    ssl_certificate_key {{.Key}};
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:SSL:10m;
    add_header Strict-Transport-Security "max-age=31536000";
{{- end}}
{{- if eq .Kind "proxy"}}

    location / {
        proxy_pass {{.Target}};
        proxy_http_version 1.1;
        proxy_set_header Host {{.ProxyHost}};
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
{{- if .UpstreamTLS}}
        proxy_ssl_server_name on;
{{- if .ProxySNI}}
        proxy_ssl_name {{.ProxySNI}};
{{- end}}
{{- end}}
{{- if .Cache}}
        proxy_cache proxy_cache_panel;
        proxy_cache_key $scheme$host$request_uri;
        proxy_cache_valid 200 301 302 {{.CacheMinutes}}m;
        proxy_cache_lock on;
        proxy_cache_use_stale error timeout updating http_500 http_502 http_503 http_504;
        # Requests from logged-in visitors must never be stored or served from the cache.
        proxy_cache_bypass $http_authorization $http_cookie;
        proxy_no_cache $http_authorization $http_cookie;
        add_header X-Cache-Status $upstream_cache_status always;
{{- else}}
        proxy_cache off;
{{- end}}
    }
{{- else}}

    root /www/sites/{{.Name}}/index;
    index index.php index.html index.htm;
{{- if eq .Kind "php"}}

    location / {
        try_files $uri $uri/ /index.php$is_args$args;
    }

    location ~ [^/]\.php(/|$) {
        fastcgi_pass {{.Target}};
        include fastcgi-php.conf;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME $document_root$fastcgi_script_name;
        fastcgi_param PATH_INFO $path_info;
    }
{{- else}}

    location / {
        try_files $uri $uri/ =404;
    }
{{- end}}
{{- end}}
}
`))

// Render validates a request and produces the vhost. Values reach the config verbatim,
// so anything that could smuggle in a directive is rejected here.
func Render(req CreateRequest) ([]byte, error) {
	if !validSite.MatchString(req.Name) {
		return nil, fmt.Errorf("site name may only contain letters, digits, dot, - and _")
	}
	if len(req.Domains) == 0 {
		return nil, fmt.Errorf("add at least one domain")
	}
	for _, d := range req.Domains {
		if !validDomain.MatchString(d) {
			return nil, fmt.Errorf("invalid domain %q", d)
		}
	}
	switch req.Kind {
	case "static":
	case "proxy":
		if !validProxy.MatchString(req.Target) {
			return nil, fmt.Errorf("proxy target must be a URL like http://127.0.0.1:3000")
		}
	case "php":
		if !validFPM.MatchString(req.Target) {
			return nil, fmt.Errorf("PHP target must be host:port, like 127.0.0.1:9000")
		}
	default:
		return nil, fmt.Errorf("kind must be static, php or proxy")
	}
	https := req.Cert != "" || req.Key != ""
	if https {
		for _, p := range []string{req.Cert, req.Key} {
			if !filepath.IsAbs(p) || strings.ContainsAny(p, " ;{}\"'\n\t") {
				return nil, fmt.Errorf("invalid certificate path %q", p)
			}
		}
	}
	proxyHost := req.ProxyHost
	if proxyHost == "" {
		proxyHost = "$host"
	}
	if !validProxyHost.MatchString(proxyHost) {
		return nil, fmt.Errorf("Host header must be $host, $proxy_host, or a host name")
	}
	minutes := req.CacheMinutes
	if minutes == 0 {
		minutes = 10
	}
	if minutes < 1 || minutes > 43200 {
		return nil, fmt.Errorf("cache time must be between 1 minute and 30 days")
	}
	// HTTPS upstreams usually host many names, so the TLS handshake has to name one too.
	// A literal Host names it; otherwise nginx sends the host from the target URL.
	sni := ""
	if !strings.HasPrefix(proxyHost, "$") {
		sni, _, _ = strings.Cut(proxyHost, ":")
	}
	var buf bytes.Buffer
	err := confTemplate.Execute(&buf, map[string]any{
		"Name": req.Name, "Domains": strings.Join(req.Domains, " "), "Kind": req.Kind,
		"Target": req.Target, "HTTPS": https, "Cert": req.Cert, "Key": req.Key,
		"ProxyHost": proxyHost, "Cache": req.Cache, "CacheMinutes": minutes,
		"UpstreamTLS": strings.HasPrefix(req.Target, "https://"), "ProxySNI": sni,
	})
	return buf.Bytes(), err
}

// connectionUpgradeMap holds the websocket map used by generated proxy sites. It sits
// in its own file because http-level maps can't be declared inside a server block.
const connectionUpgradeMap = `map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}
`

func (s *Websites) ensureUpgradeMap() error {
	path := filepath.Join(ConfDir, "00-connection-upgrade.conf")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return core.AtomicWrite(path, []byte(connectionUpgradeMap), 0o644)
}

// Apply runs nginx -t and reloads. On failure it calls undo and restores the old config.
func Apply(ctx context.Context, undo func() error) error {
	if out, err := core.Run(ctx, "nginx", "-t"); err != nil {
		if undo != nil {
			_ = undo()
		}
		return fmt.Errorf("nginx rejected the config, change reverted:\n%s", strings.TrimSpace(out))
	}
	if out, err := core.Run(ctx, "systemctl", "reload", "nginx"); err != nil {
		if undo != nil {
			_ = undo()
		}
		return fmt.Errorf("nginx reload failed, change reverted:\n%s", strings.TrimSpace(out))
	}
	return nil
}

// Both redirects above and below test the visitor's protocol, not just nginx's. Behind
// Cloudflare in Flexible mode every request reaches nginx over HTTP, so a plain
// `$scheme = http` test would redirect HTTPS visitors back to HTTPS forever.
//
// addHTTPS inserts TLS directives after server_name, leaving the rest of a config, which
// may be hand-written, untouched. A site that already has a certificate is unchanged.
func addHTTPS(conf, site string) (string, error) {
	if reCert.MatchString(conf) {
		return conf, nil
	}
	loc := reServerName.FindStringIndex(conf)
	if loc == nil {
		return "", fmt.Errorf("no server_name in config")
	}
	ssl := filepath.Join(SSLDir, site)
	block := fmt.Sprintf(`
    listen 443 ssl http2;
    ssl_certificate %s/fullchain.pem;
    ssl_certificate_key %s/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:SSL:10m;
    add_header Strict-Transport-Security "max-age=31536000";
    set $visitor_scheme "$scheme$http_x_forwarded_proto";
    if ($visitor_scheme ~ ^http(http)?$) {
        return 301 https://$host$request_uri;
    }`, ssl, ssl)
	return conf[:loc[1]] + block + conf[loc[1]:], nil
}

// EnableHTTPS points a site at the certificate in its ssl directory.
func EnableHTTPS(ctx context.Context, app *core.App, site string) error {
	path, _, err := confPath(site)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	next, err := addHTTPS(string(data), site)
	if err != nil {
		return err
	}
	if next == string(data) {
		return Apply(ctx, nil)
	}
	restore, err := app.WriteConfig(path, []byte(next), 0o644)
	if err != nil {
		return err
	}
	return Apply(ctx, restore)
}

func confPath(name string) (string, bool, error) {
	if !validSite.MatchString(name) {
		return "", false, fmt.Errorf("invalid site name")
	}
	on := filepath.Join(ConfDir, name+".conf")
	if _, err := os.Stat(on); err == nil {
		return on, true, nil
	}
	off := on + disabled
	if _, err := os.Stat(off); err == nil {
		return off, false, nil
	}
	return "", false, fmt.Errorf("site %s not found", name)
}

func (s *Websites) create(w http.ResponseWriter, r *http.Request) {
	var req CreateRequest
	if !core.Read(w, r, &req) {
		return
	}
	// A wildcard certificate that's already here means the new site can start on HTTPS.
	// Without this, a site behind a Cloudflare zone in Full mode would answer with an error.
	if req.Cert == "" && req.Key == "" {
		req.Cert, req.Key = findCert(req.Domains)
	}
	data, err := Render(req)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if _, _, err := confPath(req.Name); err == nil {
		core.Fail(w, http.StatusConflict, "site %s already exists", req.Name)
		return
	}
	root := filepath.Join(SitesDir, req.Name)
	for _, d := range []string{"index", "log"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			core.Fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
	}
	if req.Kind != "proxy" {
		index := filepath.Join(root, "index", "index.html")
		if _, err := os.Stat(index); os.IsNotExist(err) {
			_ = os.WriteFile(index, []byte("<!doctype html><title>"+req.Domains[0]+"</title><h1>"+req.Domains[0]+"</h1>\n"), 0o644)
		}
	}
	if err := s.ensureUpgradeMap(); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	restore, err := s.app.WriteConfig(filepath.Join(ConfDir, req.Name+".conf"), data, 0o644)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := Apply(r.Context(), restore); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.app.Audit(r, "site.create", req.Name)
	out := map[string]any{"ok": true, "https": req.Cert != ""}
	if req.AddDNS {
		results := cfdns.Ensure(r.Context(), s.app, req.Domains, req.Proxied)
		for _, d := range results {
			if d.Status == "created" {
				s.app.Audit(r, "dns.create", d.Domain+" "+d.Detail)
			}
		}
		out["dns"] = results
	}
	core.OK(w, out)
}

// findCert looks for an unexpired certificate in SSLDir that covers every domain.
func findCert(domains []string) (cert, key string) {
	dirs, _ := filepath.Glob(filepath.Join(SSLDir, "*"))
	for _, dir := range dirs {
		certPath, keyPath := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
		data, err := os.ReadFile(certPath)
		if err != nil {
			continue
		}
		if _, err := os.Stat(keyPath); err != nil {
			continue
		}
		if certCovers(data, domains, time.Now()) {
			return certPath, keyPath
		}
	}
	return "", ""
}

func certCovers(pemData []byte, domains []string, now time.Time) bool {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return false
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil || now.After(parsed.NotAfter) {
		return false
	}
	for _, d := range domains {
		if parsed.VerifyHostname(d) != nil {
			return false
		}
	}
	return true
}

func (s *Websites) dnsCoverage(w http.ResponseWriter, r *http.Request) {
	domains := []string{}
	for _, d := range strings.Split(r.URL.Query().Get("domains"), ",") {
		if validDomain.MatchString(d) {
			domains = append(domains, d)
		}
	}
	ip, _ := cfdns.PublicIP(r.Context(), s.app)
	cert, _ := findCert(domains)
	core.OK(w, map[string]any{
		"domains": cfdns.Check(r.Context(), s.app, domains), "serverIp": ip,
		"cert": len(domains) > 0 && cert != "",
	})
}

func (s *Websites) confGet(w http.ResponseWriter, r *http.Request) {
	path, enabled, err := confPath(chi.URLParam(r, "name"))
	if err != nil {
		core.Fail(w, http.StatusNotFound, "%v", err)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	core.OK(w, map[string]any{"content": string(data), "enabled": enabled, "path": path})
}

func (s *Websites) confSet(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	path, _, err := confPath(name)
	if err != nil {
		core.Fail(w, http.StatusNotFound, "%v", err)
		return
	}
	var req struct{ Content string }
	if !core.Read(w, r, &req) {
		return
	}
	restore, err := s.app.WriteConfig(path, []byte(req.Content), 0o644)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := Apply(r.Context(), restore); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.app.Audit(r, "site.edit", name)
	core.OK(w, map[string]bool{"ok": true})
}

func (s *Websites) toggle(w http.ResponseWriter, r *http.Request) {
	name, action := chi.URLParam(r, "name"), chi.URLParam(r, "action")
	path, enabled, err := confPath(name)
	if err != nil {
		core.Fail(w, http.StatusNotFound, "%v", err)
		return
	}
	if enabled == (action == "enable") {
		core.OK(w, map[string]bool{"ok": true})
		return
	}
	next := filepath.Join(ConfDir, name+".conf")
	if action == "disable" {
		next += disabled
	}
	if err := os.Rename(path, next); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := Apply(r.Context(), func() error { return os.Rename(next, path) }); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.app.Audit(r, "site."+action, name)
	core.OK(w, map[string]bool{"ok": true})
}

// remove deletes the vhost only. Site files stay in /www/sites unless the caller asks,
// and the old config is kept in the panel's backups directory either way.
func (s *Websites) remove(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	path, _, err := confPath(name)
	if err != nil {
		core.Fail(w, http.StatusNotFound, "%v", err)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	backup := filepath.Join(s.app.DataDir, "backups", filepath.Base(path)+".deleted")
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := os.Remove(path); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if err := Apply(r.Context(), func() error { return os.WriteFile(path, data, 0o644) }); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if r.URL.Query().Get("files") == "1" {
		// validSite was checked by confPath, so this can't escape SitesDir.
		if err := os.RemoveAll(filepath.Join(SitesDir, name)); err != nil {
			core.Fail(w, http.StatusInternalServerError, "vhost removed, but deleting files failed: %v", err)
			return
		}
	}
	s.app.Audit(r, "site.delete", name)
	core.OK(w, map[string]bool{"ok": true})
}

func (s *Websites) log(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !validSite.MatchString(name) {
		core.Fail(w, http.StatusBadRequest, "invalid site name")
		return
	}
	kind := "access"
	if r.URL.Query().Get("kind") == "error" {
		kind = "error"
	}
	lines := 500
	if n, err := strconv.Atoi(r.URL.Query().Get("lines")); err == nil && n > 0 && n <= 5000 {
		lines = n
	}
	size := logsSize(name)
	file := filepath.Join(SitesDir, name, "log", kind+".log")
	if _, err := os.Stat(file); err != nil {
		core.OK(w, map[string]any{"log": "", "size": size})
		return
	}
	out, err := core.Run(r.Context(), "tail", "-n", strconv.Itoa(lines), "--", file)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	core.OK(w, map[string]any{"log": out, "size": size})
}

// logFiles lists a site's logs, rotated copies included.
func logFiles(name string) []string {
	files, _ := filepath.Glob(filepath.Join(SitesDir, name, "log", "*.log*"))
	return files
}

func logsSize(name string) int64 {
	var total int64
	for _, f := range logFiles(name) {
		if info, err := os.Stat(f); err == nil {
			total += info.Size()
		}
	}
	return total
}

// logVacuum brings a site's logs down to keepMB in total. Rotated copies go first, since
// they're the oldest, then the live logs are trimmed in proportion to their size.
func (s *Websites) logVacuum(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !validSite.MatchString(name) {
		core.Fail(w, http.StatusBadRequest, "invalid site name")
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
	before := logsSize(name)
	var live []string
	var liveTotal int64
	for _, f := range logFiles(name) {
		if !strings.HasSuffix(f, ".log") {
			if err := os.Remove(f); err != nil {
				core.Fail(w, http.StatusInternalServerError, "%v", err)
				return
			}
			continue
		}
		info, err := os.Stat(f)
		if err != nil {
			continue
		}
		live = append(live, f)
		liveTotal += info.Size()
	}
	if liveTotal > keep {
		for _, f := range live {
			info, err := os.Stat(f)
			if err != nil {
				continue
			}
			share := int64(float64(keep) * float64(info.Size()) / float64(liveTotal))
			if _, err := core.TrimLog(f, share); err != nil {
				core.Fail(w, http.StatusInternalServerError, "%v", err)
				return
			}
		}
	}
	s.app.Audit(r, "site.logs.vacuum", name)
	size := logsSize(name)
	core.OK(w, map[string]int64{"freed": before - size, "size": size})
}

func (s *Websites) reload(w http.ResponseWriter, r *http.Request) {
	if err := Apply(r.Context(), nil); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.app.Audit(r, "nginx.reload", "")
	core.OK(w, map[string]bool{"ok": true})
}
