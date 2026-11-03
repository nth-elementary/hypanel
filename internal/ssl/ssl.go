// Package ssl manages certificates with acme.sh: issuing over HTTP or DNS validation,
// installing into a site's ssl directory, renewing, uploading, and an expiry overview.
package ssl

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
	"hypanel/internal/websites"
)

const acme = "/root/.acme.sh/acme.sh"

var (
	validDomain = regexp.MustCompile(`^(\*\.)?([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)
	validSite   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	validEmail  = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// providers maps a DNS provider to its acme.sh plugin and credential variables.
var providers = map[string]struct {
	Hook string
	Keys []string
}{
	"cloudflare": {"dns_cf", []string{"CF_Token"}},
	"dnspod":     {"dns_dp", []string{"DP_Id", "DP_Key"}},
	"tencent":    {"dns_tencent", []string{"Tencent_SecretId", "Tencent_SecretKey"}},
	"aliyun":     {"dns_ali", []string{"Ali_Key", "Ali_Secret"}},
}

type SSL struct{ app *core.App }

func New(app *core.App) (*SSL, error) {
	_, err := app.DB.Exec(`CREATE TABLE IF NOT EXISTS dns_accounts (name TEXT PRIMARY KEY, provider TEXT NOT NULL, creds TEXT NOT NULL)`)
	if err != nil {
		return nil, err
	}
	return &SSL{app: app}, nil
}

func (s *SSL) Name() string { return "ssl" }

func (s *SSL) Routes(r chi.Router) {
	r.Get("/ssl/certs", s.certs)
	r.Post("/ssl/issue", s.issue)
	r.Post("/ssl/renew", s.renew)
	r.Post("/ssl/upload", s.upload)
	r.Get("/ssl/dns", s.dnsList)
	r.Post("/ssl/dns", s.dnsSave)
	r.Delete("/ssl/dns/{name}", s.dnsDelete)
	r.Get("/ssl/providers", func(w http.ResponseWriter, _ *http.Request) {
		out := map[string][]string{}
		for k, p := range providers {
			out[k] = p.Keys
		}
		core.OK(w, out)
	})
}

type Cert struct {
	Path     string   `json:"path"`
	Sites    []string `json:"sites"`
	Domains  []string `json:"domains"`
	Issuer   string   `json:"issuer"`
	NotAfter int64    `json:"notAfter"`
	DaysLeft int      `json:"daysLeft"`
	Error    string   `json:"error,omitempty"`
}

// certs reads the certificates that enabled vhosts actually reference, so this shows
// what nginx serves regardless of how each certificate got there.
func (s *SSL) certs(w http.ResponseWriter, r *http.Request) {
	files, _ := filepath.Glob(filepath.Join(websites.ConfDir, "*.conf"))
	re := regexp.MustCompile(`(?m)^\s*ssl_certificate\s+([^;]+);`)
	bySite := map[string][]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		site := strings.TrimSuffix(filepath.Base(f), ".conf")
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			p := strings.TrimSpace(m[1])
			bySite[p] = append(bySite[p], site)
		}
	}
	out := []Cert{}
	for path, sites := range bySite {
		c := readCert(path)
		c.Sites = sites
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NotAfter < out[j].NotAfter })
	core.OK(w, out)
}

func readCert(path string) Cert {
	c := Cert{Path: path, Domains: []string{}}
	data, err := os.ReadFile(path)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	block, _ := pem.Decode(data)
	if block == nil {
		c.Error = "not a PEM certificate"
		return c
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	c.Domains = cert.DNSNames
	c.NotAfter = cert.NotAfter.Unix()
	c.DaysLeft = int(time.Until(cert.NotAfter).Hours() / 24)
	if len(cert.Issuer.Organization) > 0 {
		c.Issuer = cert.Issuer.Organization[0]
	} else {
		c.Issuer = cert.Issuer.CommonName
	}
	return c
}

// dnsEnv loads a stored DNS account as environment variables for acme.sh.
func (s *SSL) dnsEnv(account string) (hook string, env []string, err error) {
	var provider, creds string
	err = s.app.DB.QueryRow(`SELECT provider, creds FROM dns_accounts WHERE name=?`, account).Scan(&provider, &creds)
	if err != nil {
		return "", nil, fmt.Errorf("DNS account %q not found", account)
	}
	p, ok := providers[provider]
	if !ok {
		return "", nil, fmt.Errorf("unknown provider %q", provider)
	}
	values := map[string]string{}
	if err := json.Unmarshal([]byte(creds), &values); err != nil {
		return "", nil, err
	}
	for _, k := range p.Keys {
		env = append(env, k+"="+values[k])
	}
	return p.Hook, env, nil
}

// reloadCmd is a fixed string, so it can't carry anything user-supplied to the shell.
const reloadCmd = "systemctl reload nginx"

func (s *SSL) issue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Site    string   `json:"site"`
		Domains []string `json:"domains"`
		Method  string   `json:"method"` // http or a DNS account name, prefixed dns:
		Email   string   `json:"email"`
	}
	if !core.Read(w, r, &req) {
		return
	}
	if !validSite.MatchString(req.Site) {
		core.Fail(w, http.StatusBadRequest, "choose a site to install the certificate into")
		return
	}
	if len(req.Domains) == 0 {
		core.Fail(w, http.StatusBadRequest, "add at least one domain")
		return
	}
	args := []string{"--issue", "--server", "letsencrypt", "--keylength", "ec-256"}
	for _, d := range req.Domains {
		if !validDomain.MatchString(d) {
			core.Fail(w, http.StatusBadRequest, "invalid domain %q", d)
			return
		}
		args = append(args, "-d", d)
	}
	var env []string
	if account, ok := strings.CutPrefix(req.Method, "dns:"); ok {
		hook, e, err := s.dnsEnv(account)
		if err != nil {
			core.Fail(w, http.StatusBadRequest, "%v", err)
			return
		}
		args, env = append(args, "--dns", hook), e
	} else {
		for _, d := range req.Domains {
			if strings.HasPrefix(d, "*.") {
				core.Fail(w, http.StatusBadRequest, "wildcard certificates need DNS validation")
				return
			}
		}
		args = append(args, "--webroot", "/usr/share/nginx/html")
	}
	if req.Email != "" && !validEmail.MatchString(req.Email) {
		core.Fail(w, http.StatusBadRequest, "invalid email")
		return
	}

	ssl := filepath.Join(websites.SSLDir, req.Site)
	primary := req.Domains[0]
	task := s.app.Tasks.Start("Issue certificate for "+primary, func(ctx context.Context, t *core.Task) error {
		if err := os.MkdirAll(ssl, 0o700); err != nil {
			return err
		}
		if req.Email != "" {
			if err := t.Exec("", nil, acme, "--register-account", "--server", "letsencrypt", "-m", req.Email); err != nil {
				return err
			}
		}
		if err := t.Exec("", env, acme, args...); err != nil {
			return err
		}
		if err := t.Exec("", nil, acme, "--install-cert", "--ecc", "-d", primary,
			"--fullchain-file", filepath.Join(ssl, "fullchain.pem"),
			"--key-file", filepath.Join(ssl, "privkey.pem"),
			"--reloadcmd", reloadCmd); err != nil {
			return err
		}
		t.Printf("Enabling HTTPS for %s\n", req.Site)
		return websites.EnableHTTPS(ctx, s.app, req.Site)
	})
	s.app.Audit(r, "ssl.issue", strings.Join(req.Domains, ","))
	core.OK(w, map[string]string{"task": task.ID})
}

func (s *SSL) renew(w http.ResponseWriter, r *http.Request) {
	var req struct{ Domain string }
	if !core.Read(w, r, &req) {
		return
	}
	if !validDomain.MatchString(req.Domain) {
		core.Fail(w, http.StatusBadRequest, "invalid domain")
		return
	}
	task := s.app.Tasks.Start("Renew "+req.Domain, func(_ context.Context, t *core.Task) error {
		return t.Exec("", nil, acme, "--renew", "--force", "--ecc", "-d", req.Domain)
	})
	s.app.Audit(r, "ssl.renew", req.Domain)
	core.OK(w, map[string]string{"task": task.ID})
}

// upload installs a certificate after checking the key belongs to it, so a mismatch
// is reported here rather than breaking the nginx reload.
func (s *SSL) upload(w http.ResponseWriter, r *http.Request) {
	var req struct{ Site, Cert, Key string }
	if !core.Read(w, r, &req) {
		return
	}
	if !validSite.MatchString(req.Site) {
		core.Fail(w, http.StatusBadRequest, "invalid site name")
		return
	}
	if _, err := tls.X509KeyPair([]byte(req.Cert), []byte(req.Key)); err != nil {
		core.Fail(w, http.StatusBadRequest, "certificate and key don't match: %v", err)
		return
	}
	dir := filepath.Join(websites.SSLDir, req.Site)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	restoreCert, err := s.app.WriteConfig(filepath.Join(dir, "fullchain.pem"), []byte(req.Cert), 0o644)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	restoreKey, err := s.app.WriteConfig(filepath.Join(dir, "privkey.pem"), []byte(req.Key), 0o600)
	if err != nil {
		_ = restoreCert()
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	undo := func() error { _ = restoreCert(); return restoreKey() }
	if err := websites.Apply(r.Context(), undo); err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := websites.EnableHTTPS(r.Context(), s.app, req.Site); err != nil {
		core.Fail(w, http.StatusBadRequest, "certificate saved, but enabling HTTPS failed: %v", err)
		return
	}
	s.app.Audit(r, "ssl.upload", req.Site)
	core.OK(w, map[string]bool{"ok": true})
}

// dnsList returns account names only. Stored credentials never leave the server.
func (s *SSL) dnsList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.app.DB.Query(`SELECT name, provider FROM dns_accounts ORDER BY name`)
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var name, provider string
		if rows.Scan(&name, &provider) == nil {
			out = append(out, map[string]string{"name": name, "provider": provider})
		}
	}
	core.OK(w, out)
}

func (s *SSL) dnsSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string            `json:"name"`
		Provider string            `json:"provider"`
		Creds    map[string]string `json:"creds"`
	}
	if !core.Read(w, r, &req) {
		return
	}
	p, ok := providers[req.Provider]
	if !ok || !validSite.MatchString(req.Name) {
		core.Fail(w, http.StatusBadRequest, "invalid name or provider")
		return
	}
	for _, k := range p.Keys {
		if req.Creds[k] == "" {
			core.Fail(w, http.StatusBadRequest, "missing %s", k)
			return
		}
	}
	creds, err := json.Marshal(req.Creds)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	_, err = s.app.DB.Exec(`INSERT INTO dns_accounts(name,provider,creds) VALUES(?,?,?)
		ON CONFLICT(name) DO UPDATE SET provider=excluded.provider, creds=excluded.creds`, req.Name, req.Provider, string(creds))
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	s.app.Audit(r, "ssl.dns.save", req.Name)
	core.OK(w, map[string]bool{"ok": true})
}

func (s *SSL) dnsDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, err := s.app.DB.Exec(`DELETE FROM dns_accounts WHERE name=?`, name); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	s.app.Audit(r, "ssl.dns.delete", name)
	core.OK(w, map[string]bool{"ok": true})
}
