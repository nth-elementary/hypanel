package cron

import "testing"

func TestFormatParseRoundTrip(t *testing.T) {
	in := Job{ID: "a1", Name: "Sitemap", Spec: "30 3 * * *", Command: `docker exec x sh -c 'date +%F  >> /tmp/o'`}
	out, ok := parse("a1", string(format(in)))
	if !ok {
		t.Fatal("parse failed")
	}
	if out.Name != in.Name || out.Spec != in.Spec || out.Command != in.Command {
		t.Errorf("got %+v", out)
	}
}

func TestParseSpecialSchedule(t *testing.T) {
	j, ok := parse("b", "# name: Boot\n@reboot root /usr/local/bin/x --flag\n")
	if !ok || j.Spec != "@reboot" || j.Command != "/usr/local/bin/x --flag" {
		t.Errorf("%+v %v", j, ok)
	}
}

func TestSpecValidation(t *testing.T) {
	for spec, want := range map[string]bool{
		"30 3 * * *":   true,
		"*/5 * * * *":  true,
		"0 12 * * mon": true,
		"@daily":       true,
		"* * * *":      false,
		"* * * * * *":  false,
		"; rm -rf / #": false,
		"@sometimes":   false,
	} {
		if got := validSpec.MatchString(spec); got != want {
			t.Errorf("%q: got %v want %v", spec, got, want)
		}
	}
}
