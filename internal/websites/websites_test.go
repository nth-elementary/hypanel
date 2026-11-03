package websites

import (
	"strings"
	"testing"
)

func TestRenderProxy(t *testing.T) {
	got, err := Render(CreateRequest{Name: "app", Domains: []string{"app.example.org"}, Kind: "proxy", Target: "http://127.0.0.1:3000"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"server_name app.example.org;", "proxy_pass http://127.0.0.1:3000;", "/www/sites/app/log/access.log"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "listen 443") {
		t.Error("no certificate given, should not listen on 443")
	}
}

func TestRenderPHPWithTLS(t *testing.T) {
	got, err := Render(CreateRequest{
		Name: "blog", Domains: []string{"example.org", "www.example.org"}, Kind: "php", Target: "127.0.0.1:9000",
		Cert: "/www/sites/blog/ssl/fullchain.pem", Key: "/www/sites/blog/ssl/privkey.pem",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"listen 443 ssl http2;", "fastcgi_pass 127.0.0.1:9000;", "server_name example.org www.example.org;", "ssl_certificate_key /www/sites/blog/ssl/privkey.pem;"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Values go into nginx config verbatim, so directive injection must be impossible.
func TestRenderRejectsInjection(t *testing.T) {
	for name, req := range map[string]CreateRequest{
		"domain":  {Name: "a", Domains: []string{"a.org; include /etc/passwd"}, Kind: "static"},
		"name":    {Name: "../etc", Domains: []string{"a.org"}, Kind: "static"},
		"proxy":   {Name: "a", Domains: []string{"a.org"}, Kind: "proxy", Target: "http://x; }"},
		"php":     {Name: "a", Domains: []string{"a.org"}, Kind: "php", Target: "x:1;"},
		"cert":    {Name: "a", Domains: []string{"a.org"}, Kind: "static", Cert: "/a; b", Key: "/k"},
		"kind":    {Name: "a", Domains: []string{"a.org"}, Kind: "other"},
		"noCertK": {Name: "a", Domains: []string{"a.org"}, Kind: "static", Cert: "/a"},
	} {
		if _, err := Render(req); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestParseSite(t *testing.T) {
	conf := `server {
    listen 443 ssl http2;
    server_name blog.example.org example.org;
    root /www/sites/blog.example.org/index;
    location ~ [^/]\.php(/|$) {
        fastcgi_pass 127.0.0.1:9000;
    }
    ssl_certificate /www/sites/blog.example.org/ssl/fullchain.pem;
}`
	s, ok := parseSite("blog.example.org", conf)
	if !ok || s.Kind != "php" || s.Target != "127.0.0.1:9000" || !s.HTTPS || len(s.Domains) != 2 {
		t.Errorf("%+v", s)
	}
	if _, ok := parseSite("map", "map $a $b { default 1; }"); ok {
		t.Error("a map-only file is not a site")
	}
}

func TestAddHTTPS(t *testing.T) {
	conf := "server {\n    listen 80;\n    server_name a.org;\n    root /x;\n}\n"
	got, err := addHTTPS(conf, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ssl_certificate /etc/nginx/ssl/a/fullchain.pem;") || !strings.Contains(got, "listen 443 ssl http2;") {
		t.Errorf("missing TLS directives:\n%s", got)
	}
	again, err := addHTTPS(got, "a")
	if err != nil || again != got {
		t.Error("applying twice should change nothing")
	}
}

// A redirect on $scheme alone loops forever behind Cloudflare's Flexible mode.
func TestHTTPSRedirectIsSafeBehindProxy(t *testing.T) {
	const safe = "$visitor_scheme ~ ^http(http)?$"
	got, err := Render(CreateRequest{Name: "a", Domains: []string{"a.org"}, Kind: "static", Cert: "/c", Key: "/k"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), safe) || strings.Contains(string(got), "$scheme = http") {
		t.Errorf("template:\n%s", got)
	}
	added, err := addHTTPS("server {\n    server_name a.org;\n}\n", "a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(added, safe) || strings.Contains(added, "$scheme = http") {
		t.Errorf("addHTTPS:\n%s", added)
	}
}

func TestRenderProxyOptions(t *testing.T) {
	got, err := Render(CreateRequest{
		Name: "m", Domains: []string{"m.example.org"}, Kind: "proxy", Target: "https://upstream.example",
		ProxyHost: "upstream.example", Cache: true, CacheMinutes: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"proxy_set_header Host upstream.example;", "proxy_ssl_server_name on;", "proxy_ssl_name upstream.example;",
		"proxy_cache proxy_cache_panel;", "proxy_cache_valid 200 301 302 30m;", "proxy_no_cache $http_authorization $http_cookie;",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "proxy_cache off") {
		t.Error("cache was requested")
	}

	plain, err := Render(CreateRequest{Name: "p", Domains: []string{"p.example.org"}, Kind: "proxy", Target: "http://127.0.0.1:3000"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"proxy_set_header Host $host;", "proxy_cache off;"} {
		if !strings.Contains(string(plain), want) {
			t.Errorf("default proxy: missing %q", want)
		}
	}
	if strings.Contains(string(plain), "proxy_ssl_server_name") {
		t.Error("plain HTTP upstream needs no TLS settings")
	}

	for name, req := range map[string]CreateRequest{
		"host injection": {ProxyHost: "x; include /etc/passwd"},
		"host variable":  {ProxyHost: "$request_uri"},
		"cache too long": {Cache: true, CacheMinutes: 99999999},
	} {
		req.Name, req.Domains, req.Kind, req.Target = "a", []string{"a.org"}, "proxy", "http://127.0.0.1:1"
		if _, err := Render(req); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// Migrated vhosts keep a root inside their acme-challenge location, which comes first.
func TestParseSiteUsesServerLevelRoot(t *testing.T) {
	conf := `server {
    listen 80;
    server_name a.org;
    # root /commented/out;
    location ^~ /.well-known/acme-challenge {
        allow all;
        root /usr/share/nginx/html;
    }
    root /www/sites/a/index;
}`
	s, ok := parseSite("a", conf)
	if !ok || s.Kind != "static" || s.Target != "/www/sites/a/index" {
		t.Errorf("%+v", s)
	}
}
