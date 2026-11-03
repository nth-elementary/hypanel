package cfdns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hypanel/internal/core"
)

func TestMatchZone(t *testing.T) {
	zones := []zone{{Name: "example.org"}, {Name: "example.net"}, {Name: "sub.example.net"}}
	for domain, want := range map[string]string{
		"example.org":          "example.org",
		"a.example.org":        "example.org",
		"A.B.EXAMPLE.ORG":      "example.org",
		"x.example.net":     "example.net",
		"x.sub.example.net": "sub.example.net",
		"notexample.org":       "",
		"example.com":      "",
	} {
		z, ok := matchZone(zones, domain)
		if ok != (want != "") || z.Name != want {
			t.Errorf("%s: got %q (%v), want %q", domain, z.Name, ok, want)
		}
	}
}

func TestEnsure(t *testing.T) {
	created := []map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing token on %s", r.URL)
		}
		reply := func(v any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": v})
		}
		switch {
		case r.URL.Path == "/zones":
			reply([]map[string]string{{"id": "z1", "name": "example.org"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/dns_records"):
			if r.URL.Query().Get("name") == "taken.example.org" {
				reply([]map[string]string{{"type": "CNAME", "content": "elsewhere.example"}})
				return
			}
			reply([]map[string]string{})
		case r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			created = append(created, body)
			reply(body)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	defer srv.Close()
	apiBase = srv.URL

	app, err := core.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(`CREATE TABLE dns_accounts (name TEXT PRIMARY KEY, provider TEXT, creds TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.DB.Exec(`INSERT INTO dns_accounts VALUES ('cf','cloudflare','{"CF_Token":"tok"}')`); err != nil {
		t.Fatal(err)
	}
	if err := app.SetSetting("public_ip", "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	cache.zones, cache.fetched = nil, time.Time{}

	got := Ensure(context.Background(), app, []string{"new.example.org", "taken.example.org", "other.example", "*.example.org"}, true)
	want := map[string]string{"new.example.org": "created", "taken.example.org": "exists", "other.example": "skipped", "*.example.org": "skipped"}
	for _, r := range got {
		if r.Status != want[r.Domain] {
			t.Errorf("%s: got %s (%s), want %s", r.Domain, r.Status, r.Detail, want[r.Domain])
		}
	}
	if len(created) != 1 || created[0]["name"] != "new.example.org" || created[0]["content"] != "203.0.113.9" || created[0]["proxied"] != true {
		t.Errorf("created: %+v", created)
	}

	check := Check(context.Background(), app, []string{"taken.example.org", "other.example"})
	if !check[0].Covered || check[0].Existing == "" || check[1].Covered {
		t.Errorf("check: %+v", check)
	}
}
