package runtimes

import (
	"strings"
	"testing"
)

func TestRenderNode(t *testing.T) {
	files, err := Render(Request{Kind: "node", Name: "app", Version: "22", Port: 3100, AppPort: 3000, Code: "/srv/app", Command: `npm ci && npm start -- --title "x"`})
	if err != nil {
		t.Fatal(err)
	}
	c := string(files["docker-compose.yml"])
	for _, want := range []string{"image: node:22-bookworm-slim", "127.0.0.1:3100:3000", "- hypanel-network", "/srv/app:/app", `"npm ci && npm start -- --title \"x\""`} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestRenderPHP(t *testing.T) {
	files, err := Render(Request{Kind: "php", Name: "site", Version: "8.4", Port: 9100, Extensions: []string{"imagick"}})
	if err != nil {
		t.Fatal(err)
	}
	if d := string(files["Dockerfile"]); !strings.Contains(d, "php:8.4-fpm-alpine") || !strings.Contains(d, "imagick") {
		t.Errorf("Dockerfile:\n%s", d)
	}
	if !strings.Contains(string(files["docker-compose.yml"]), "127.0.0.1:9100:9000") {
		t.Error("port should bind to loopback")
	}
}

func TestRenderMariaDBGeneratesPassword(t *testing.T) {
	a, err := Render(Request{Kind: "mariadb", Name: "db", Version: "11.4", Port: 3307})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Render(Request{Kind: "mariadb", Name: "db", Version: "11.4", Port: 3307})
	if string(a[".env"]) == string(b[".env"]) {
		t.Error("each instance should get its own password")
	}
	if strings.Contains(string(a["docker-compose.yml"]), string(a[".env"])) {
		t.Error("password belongs in .env, not the compose file")
	}
}

func TestEveryStackLimitsItsLogs(t *testing.T) {
	for _, req := range []Request{
		{Kind: "php", Name: "a", Version: "8.4", Port: 9100},
		{Kind: "node", Name: "a", Version: "22", Port: 3100, AppPort: 3000, Code: "/a", Command: "x"},
		{Kind: "python", Name: "a", Version: "3.13", Port: 8100, AppPort: 8000, Code: "/a", Command: "x"},
		{Kind: "mariadb", Name: "a", Version: "11.4", Port: 3307},
	} {
		files, err := Render(req)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(files["docker-compose.yml"]), "max-size: 50m") {
			t.Errorf("%s: no log limit", req.Kind)
		}
	}
}

func TestRenderRejectsBadInput(t *testing.T) {
	for name, req := range map[string]Request{
		"kind":      {Kind: "ruby", Name: "a", Version: "3", Port: 3000},
		"name":      {Kind: "mariadb", Name: "../x", Version: "11", Port: 3307},
		"version":   {Kind: "mariadb", Name: "a", Version: "11; rm -rf /", Port: 3307},
		"lowPort":   {Kind: "mariadb", Name: "a", Version: "11", Port: 80},
		"code":      {Kind: "node", Name: "a", Version: "22", Port: 3000, AppPort: 3000, Code: "/a:/etc", Command: "x"},
		"relative":  {Kind: "node", Name: "a", Version: "22", Port: 3000, AppPort: 3000, Code: "app", Command: "x"},
		"newline":   {Kind: "node", Name: "a", Version: "22", Port: 3000, AppPort: 3000, Code: "/a", Command: "x\ny"},
		"extension": {Kind: "php", Name: "a", Version: "8.4", Port: 9100, Extensions: []string{"gd; id"}},
	} {
		if _, err := Render(req); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}
