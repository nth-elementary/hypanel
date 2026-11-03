package auth

import (
	"encoding/base32"
	"net/http/httptest"
	"testing"
	"time"
)

// RFC 6238 appendix B vector for SHA-1 at T=59s, truncated to 6 digits.
func TestTOTPVector(t *testing.T) {
	if got := totpAt([]byte("12345678901234567890"), 59/30); got != "287082" {
		t.Fatalf("got %s", got)
	}
}

func TestValidTOTPWindow(t *testing.T) {
	key := []byte("12345678901234567890")
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
	now := time.Unix(90000, 0)
	step := now.Unix() / 30
	for _, c := range []struct {
		code string
		want bool
	}{
		{totpAt(key, step), true},
		{totpAt(key, step-1), true},
		{totpAt(key, step+1), true},
		{totpAt(key, step-5), false},
		{"12345", false},
	} {
		if got := validTOTP(secret, c.code, now); got != c.want {
			t.Errorf("code %q: got %v want %v", c.code, got, c.want)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	r := httptest.NewRequest("POST", "https://panel.example.org/api/x", nil)
	if !SameOrigin(r) {
		t.Error("a missing Origin should pass")
	}
	r.Header.Set("Origin", "https://panel.example.org")
	if !SameOrigin(r) {
		t.Error("matching Origin should pass")
	}
	r.Header.Set("Origin", "https://evil.example")
	if SameOrigin(r) {
		t.Error("foreign Origin should fail")
	}
}
