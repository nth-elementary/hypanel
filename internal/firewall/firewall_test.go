package firewall

import (
	"strings"
	"testing"
)

const sample = `Status: active

     To                         Action      From
     --                         ------      ----
[ 1] 22,80,443,3306/tcp         DENY IN     57.141.0.0/24
[ 2] 80,443/tcp                 DENY IN     103.218.241.71
[ 3] 22/tcp                     ALLOW IN    Anywhere
[ 4] 443/tcp                    ALLOW IN    Anywhere                   # https
[ 5] 22/tcp (v6)                ALLOW IN    Anywhere (v6)
`

func TestParseStatus(t *testing.T) {
	active, rules := parseStatus(sample)
	if !active {
		t.Fatal("expected active")
	}
	if len(rules) != 5 {
		t.Fatalf("got %d rules: %+v", len(rules), rules)
	}
	if r := rules[0]; r.Num != 1 || r.To != "22,80,443,3306/tcp" || r.Action != "DENY IN" || r.From != "57.141.0.0/24" {
		t.Errorf("rule 1: %+v", r)
	}
	if r := rules[3]; r.From != "Anywhere" || r.To != "443/tcp" {
		t.Errorf("comment should be stripped: %+v", r)
	}
	if r := rules[4]; !r.V6 || r.To != "22/tcp" || r.From != "Anywhere" {
		t.Errorf("v6 rule: %+v", r)
	}
	if active, _ := parseStatus("Status: inactive\n"); active {
		t.Error("inactive parsed as active")
	}
}

func TestLocksOutSSH(t *testing.T) {
	_, rules := parseStatus(sample)
	if !locksOutSSH(rules, 3) {
		t.Error("removing the only IPv4 SSH rule should be refused")
	}
	if locksOutSSH(rules, 4) {
		t.Error("removing an unrelated rule should be fine")
	}
}

func TestRuleArgs(t *testing.T) {
	got, err := ruleArgs("allow", "8080", "tcp", "")
	if err != nil || strings.Join(got, " ") != "allow from any to any port 8080 proto tcp" {
		t.Errorf("got %v %v", got, err)
	}
	got, err = ruleArgs("deny", "", "", "1.2.3.0/24")
	if err != nil || strings.Join(got, " ") != "insert 1 deny from 1.2.3.0/24" {
		t.Errorf("got %v %v", got, err)
	}
	for _, c := range [][4]string{
		{"allow", "22; rm -rf /", "", ""},
		{"allow", "80", "icmp", ""},
		{"allow", "", "", ""},
		{"drop", "80", "", ""},
		{"allow", "80", "", "not-an-ip"},
		{"allow", "1000:2000", "", ""},
	} {
		if _, err := ruleArgs(c[0], c[1], c[2], c[3]); err == nil {
			t.Errorf("expected error for %v", c)
		}
	}
}
