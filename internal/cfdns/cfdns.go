// Package cfdns creates DNS records through the Cloudflare accounts stored by the SSL
// module. It only ever adds missing records: existing ones are reported, never changed.
package cfdns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"hypanel/internal/core"
)

// Variables so tests can point at a fake server.
var (
	apiBase  = "https://api.cloudflare.com/client/v4"
	traceURL = "https://1.1.1.1/cdn-cgi/trace"
	client   = &http.Client{Timeout: 15 * time.Second}
)

const zoneCacheTTL = 5 * time.Minute

type zone struct{ ID, Name, Token, Account string }

var cache struct {
	sync.Mutex
	zones   []zone
	fetched time.Time
	ip      string
}

// Coverage describes how one domain relates to the stored accounts.
type Coverage struct {
	Domain   string `json:"domain"`
	Covered  bool   `json:"covered"`
	Zone     string `json:"zone,omitempty"`
	Account  string `json:"account,omitempty"`
	Existing string `json:"existing,omitempty"` // what the name already points at
}

// Result is the outcome of trying to add one record.
type Result struct {
	Domain string `json:"domain"`
	Status string `json:"status"` // created, exists, skipped, failed
	Detail string `json:"detail,omitempty"`
}

func call(ctx context.Context, token, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare: unreadable response (HTTP %d)", resp.StatusCode)
	}
	if !env.Success {
		if len(env.Errors) > 0 {
			return fmt.Errorf("cloudflare: %s", env.Errors[0].Message)
		}
		return fmt.Errorf("cloudflare: HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// loadZones lists the zones every stored account can see, cached briefly so typing in
// the new-website form doesn't hit the API repeatedly.
func loadZones(ctx context.Context, app *core.App) []zone {
	cache.Lock()
	defer cache.Unlock()
	if cache.zones != nil && time.Since(cache.fetched) < zoneCacheTTL {
		return cache.zones
	}
	zones := []zone{}
	rows, err := app.DB.QueryContext(ctx, `SELECT name, creds FROM dns_accounts WHERE provider='cloudflare' ORDER BY name`)
	if err != nil {
		return zones
	}
	type account struct{ name, token string }
	accounts := []account{}
	for rows.Next() {
		var name, creds string
		if rows.Scan(&name, &creds) != nil {
			continue
		}
		var c struct{ CF_Token string }
		if json.Unmarshal([]byte(creds), &c) == nil && c.CF_Token != "" {
			accounts = append(accounts, account{name, c.CF_Token})
		}
	}
	rows.Close()
	for _, a := range accounts {
		var result []struct{ ID, Name string }
		if err := call(ctx, a.token, http.MethodGet, "/zones?per_page=50", nil, &result); err != nil {
			continue // one bad token shouldn't hide the other accounts' zones
		}
		for _, z := range result {
			zones = append(zones, zone{z.ID, z.Name, a.token, a.name})
		}
	}
	cache.zones, cache.fetched = zones, time.Now()
	return zones
}

// matchZone finds the longest zone name that is the domain or a parent of it. The dot
// matters: notexample.org must not match the zone example.org.
func matchZone(zones []zone, domain string) (zone, bool) {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	var best zone
	found := false
	for _, z := range zones {
		name := strings.ToLower(z.Name)
		if (domain == name || strings.HasSuffix(domain, "."+name)) && len(name) > len(best.Name) {
			best, found = z, true
		}
	}
	return best, found
}

func existing(ctx context.Context, z zone, domain string) (string, error) {
	var records []struct{ Type, Content string }
	q := url.Values{"name": {domain}, "per_page": {"20"}}
	if err := call(ctx, z.Token, http.MethodGet, "/zones/"+z.ID+"/dns_records?"+q.Encode(), nil, &records); err != nil {
		return "", err
	}
	for _, r := range records {
		switch r.Type {
		case "A", "AAAA", "CNAME":
			return r.Type + " " + r.Content, nil
		}
	}
	return "", nil
}

// PublicIP asks Cloudflare what address this server connects from. The cloud VM's own
// interface usually holds a private address, so it can't simply be read locally.
func PublicIP(ctx context.Context, app *core.App) (string, error) {
	if ip := app.Setting("public_ip"); ip != "" {
		return ip, nil
	}
	cache.Lock()
	known := cache.ip
	cache.Unlock()
	if known != "" {
		return known, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, traceURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "ip="); ok {
			if parsed := net.ParseIP(strings.TrimSpace(v)); parsed != nil && parsed.To4() != nil {
				cache.Lock()
				cache.ip = parsed.String()
				cache.Unlock()
				return parsed.String(), nil
			}
		}
	}
	return "", fmt.Errorf("couldn't determine this server's public IPv4 address")
}

// Check reports whether each domain is covered, for the form to show before creating.
func Check(ctx context.Context, app *core.App, domains []string) []Coverage {
	zones := loadZones(ctx, app)
	out := make([]Coverage, 0, len(domains))
	for _, d := range domains {
		c := Coverage{Domain: d}
		if z, ok := matchZone(zones, d); ok && !strings.HasPrefix(d, "*.") {
			c.Covered, c.Zone, c.Account = true, z.Name, z.Account
			c.Existing, _ = existing(ctx, z, d)
		}
		out = append(out, c)
	}
	return out
}

// Ensure adds an A record for each domain that's covered and has none. A failure here
// is reported rather than returned as an error: the website itself was still created.
func Ensure(ctx context.Context, app *core.App, domains []string, proxied bool) []Result {
	zones := loadZones(ctx, app)
	out := make([]Result, 0, len(domains))
	for _, d := range domains {
		z, ok := matchZone(zones, d)
		if !ok {
			out = append(out, Result{d, "skipped", "no stored Cloudflare account covers this domain"})
			continue
		}
		if strings.HasPrefix(d, "*.") {
			out = append(out, Result{d, "skipped", "wildcard records aren't added automatically"})
			continue
		}
		have, err := existing(ctx, z, d)
		if err != nil {
			out = append(out, Result{d, "failed", err.Error()})
			continue
		}
		if have != "" {
			out = append(out, Result{d, "exists", have})
			continue
		}
		ip, err := PublicIP(ctx, app)
		if err != nil {
			out = append(out, Result{d, "failed", err.Error()})
			continue
		}
		body := map[string]any{"type": "A", "name": d, "content": ip, "proxied": proxied, "ttl": 1}
		if err := call(ctx, z.Token, http.MethodPost, "/zones/"+z.ID+"/dns_records", body, nil); err != nil {
			out = append(out, Result{d, "failed", err.Error()})
			continue
		}
		out = append(out, Result{d, "created", "A " + ip})
	}
	return out
}
