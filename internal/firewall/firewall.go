// Package firewall wraps ufw. It also lists docker-published ports, because docker
// writes its own iptables rules and those ports are reachable regardless of ufw.
package firewall

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"hypanel/internal/core"
)

type Firewall struct{ app *core.App }

func New(app *core.App) *Firewall { return &Firewall{app: app} }

func (f *Firewall) Name() string { return "firewall" }

func (f *Firewall) Routes(r chi.Router) {
	r.Get("/firewall/status", f.status)
	r.Post("/firewall/rules", f.add)
	r.Delete("/firewall/rules/{num}", f.remove)
	r.Post("/firewall/enable", f.enable)
	r.Post("/firewall/disable", f.disable)
}

type Rule struct {
	Num    int    `json:"num"`
	To     string `json:"to"`
	Action string `json:"action"`
	From   string `json:"from"`
	V6     bool   `json:"v6"`
	Raw    string `json:"raw"`
}

var ruleLine = regexp.MustCompile(`^\[\s*(\d+)\]\s+(.+?)\s{2,}((?:ALLOW|DENY|REJECT|LIMIT)(?:\s+(?:IN|OUT|FWD))?)\s{2,}(.+?)\s*(?:#.*)?$`)

// parseStatus reads `ufw status numbered`.
func parseStatus(out string) (active bool, rules []Rule) {
	rules = []Rule{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, " ")
		if strings.HasPrefix(line, "Status:") {
			active = strings.Contains(line, "active") && !strings.Contains(line, "inactive")
			continue
		}
		m := ruleLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		num, _ := strconv.Atoi(m[1])
		to := m[2]
		v6 := strings.Contains(to, "(v6)") || strings.Contains(m[4], "(v6)")
		rules = append(rules, Rule{
			Num: num, Action: strings.Join(strings.Fields(m[3]), " "), V6: v6, Raw: line,
			To:   strings.TrimSpace(strings.ReplaceAll(to, "(v6)", "")),
			From: strings.TrimSpace(strings.ReplaceAll(m[4], "(v6)", "")),
		})
	}
	return active, rules
}

func (f *Firewall) status(w http.ResponseWriter, r *http.Request) {
	out, err := core.Run(r.Context(), "ufw", "status", "numbered")
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "ufw: %v", err)
		return
	}
	active, rules := parseStatus(out)
	core.OK(w, map[string]any{"active": active, "rules": rules, "dockerPorts": dockerPublicPorts(r.Context())})
}

type dockerPort struct {
	Container string `json:"container"`
	Port      string `json:"port"`
}

// dockerPublicPorts lists published ports not bound to loopback.
func dockerPublicPorts(ctx context.Context) []dockerPort {
	out := []dockerPort{}
	raw, err := core.Run(ctx, "docker", "ps", "--format", "{{json .}}")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var c struct{ Names, Ports string }
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		for _, p := range strings.Split(c.Ports, ", ") {
			if strings.Contains(p, "->") && !strings.HasPrefix(p, "127.") && !strings.HasPrefix(p, "[::1]") {
				out = append(out, dockerPort{c.Names, p})
			}
		}
	}
	return out
}

var portSpec = regexp.MustCompile(`^\d{1,5}(:\d{1,5})?$`)

// ruleArgs validates a request and builds the ufw arguments. Everything is checked
// here because the values become command-line arguments.
func ruleArgs(action, port, proto, from string) ([]string, error) {
	if action != "allow" && action != "deny" {
		return nil, fmt.Errorf("action must be allow or deny")
	}
	if port != "" && !portSpec.MatchString(port) {
		return nil, fmt.Errorf("invalid port %q", port)
	}
	if proto != "" && proto != "tcp" && proto != "udp" {
		return nil, fmt.Errorf("protocol must be tcp, udp or empty")
	}
	if strings.Contains(port, ":") && proto == "" {
		return nil, fmt.Errorf("a port range needs a protocol")
	}
	if from == "" {
		from = "any"
	}
	if from != "any" && net.ParseIP(from) == nil {
		if _, _, err := net.ParseCIDR(from); err != nil {
			return nil, fmt.Errorf("source must be an IP, a CIDR range or empty")
		}
	}
	if port == "" && from == "any" {
		return nil, fmt.Errorf("give a port or a source address")
	}
	// insert 1 puts deny rules ahead of the allows, or they would never match.
	var args []string
	if action == "deny" {
		args = []string{"insert", "1"}
	}
	args = append(args, action, "from", from)
	if port != "" {
		args = append(args, "to", "any", "port", port)
		if proto != "" {
			args = append(args, "proto", proto)
		}
	}
	return args, nil
}

func (f *Firewall) add(w http.ResponseWriter, r *http.Request) {
	var req struct{ Action, Port, Proto, From string }
	if !core.Read(w, r, &req) {
		return
	}
	args, err := ruleArgs(req.Action, req.Port, req.Proto, req.From)
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if out, err := core.Run(r.Context(), "ufw", args...); err != nil {
		core.Fail(w, http.StatusBadRequest, "%s", strings.TrimSpace(out))
		return
	}
	f.app.Audit(r, "firewall.add", strings.Join(args, " "))
	core.OK(w, map[string]bool{"ok": true})
}

// remove takes the rule text as well as its number. Numbers shift whenever a rule
// changes, so this confirms the number still points at the rule the user saw.
func (f *Firewall) remove(w http.ResponseWriter, r *http.Request) {
	num, err := strconv.Atoi(chi.URLParam(r, "num"))
	if err != nil {
		core.Fail(w, http.StatusBadRequest, "invalid rule number")
		return
	}
	expect := r.URL.Query().Get("raw")
	out, err := core.Run(r.Context(), "ufw", "status", "numbered")
	if err != nil {
		core.Fail(w, http.StatusInternalServerError, "ufw: %v", err)
		return
	}
	active, rules := parseStatus(out)
	var target *Rule
	for i := range rules {
		if rules[i].Num == num {
			target = &rules[i]
		}
	}
	if target == nil || target.Raw != expect {
		core.Fail(w, http.StatusConflict, "firewall rules changed, reload and try again")
		return
	}
	if active && locksOutSSH(rules, num) {
		core.Fail(w, http.StatusForbidden, "this is the last rule allowing SSH; removing it could lock you out")
		return
	}
	if out, err := core.Run(r.Context(), "ufw", "--force", "delete", strconv.Itoa(num)); err != nil {
		core.Fail(w, http.StatusBadRequest, "%s", strings.TrimSpace(out))
		return
	}
	f.app.Audit(r, "firewall.delete", target.Raw)
	core.OK(w, map[string]bool{"ok": true})
}

// sshPort reads the port sshd listens on, defaulting to 22.
func sshPort() string {
	data, err := os.ReadFile("/etc/ssh/sshd_config")
	if err != nil {
		return "22"
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.EqualFold(fields[0], "Port") {
			return fields[1]
		}
	}
	return "22"
}

func allowsSSH(rule Rule, port string) bool {
	if !strings.HasPrefix(rule.Action, "ALLOW") || rule.V6 || rule.From != "Anywhere" {
		return false
	}
	for _, p := range strings.Split(strings.TrimSuffix(strings.TrimSuffix(rule.To, "/tcp"), "/udp"), ",") {
		if p == port {
			return !strings.HasSuffix(rule.To, "/udp")
		}
	}
	return false
}

// locksOutSSH reports whether SSH would have no open-to-anywhere rule without rule num.
func locksOutSSH(rules []Rule, num int) bool {
	port := sshPort()
	had, remains := false, false
	for _, rule := range rules {
		if allowsSSH(rule, port) {
			had = true
			if rule.Num != num {
				remains = true
			}
		}
	}
	return had && !remains
}

// enable always allows SSH first, so turning the firewall on can't cut off access.
func (f *Firewall) enable(w http.ResponseWriter, r *http.Request) {
	if out, err := core.Run(r.Context(), "ufw", "allow", sshPort()+"/tcp"); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%s", strings.TrimSpace(out))
		return
	}
	if out, err := core.Run(r.Context(), "ufw", "--force", "enable"); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%s", strings.TrimSpace(out))
		return
	}
	f.app.Audit(r, "firewall.enable", "")
	core.OK(w, map[string]bool{"ok": true})
}

func (f *Firewall) disable(w http.ResponseWriter, r *http.Request) {
	if out, err := core.Run(r.Context(), "ufw", "disable"); err != nil {
		core.Fail(w, http.StatusInternalServerError, "%s", strings.TrimSpace(out))
		return
	}
	f.app.Audit(r, "firewall.disable", "")
	core.OK(w, map[string]bool{"ok": true})
}
