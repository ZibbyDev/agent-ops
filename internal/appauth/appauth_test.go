package appauth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func req(headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func basicHeader(user, pass string) string {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth(user, pass)
	return r.Header.Get("Authorization")
}

func TestPassthroughWhenNil(t *testing.T) {
	current.Store(nil)
	ok, status, _ := Load().Enforce(req(nil))
	if !ok || status != 0 {
		t.Fatalf("nil config must pass through, got ok=%v status=%d", ok, status)
	}
}

func TestEmptyConfigIsPassthrough(t *testing.T) {
	if err := Apply([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if Load() != nil {
		t.Fatal("empty config should store nil (passthrough)")
	}
	if err := Apply([]byte(`null`)); err != nil {
		t.Fatal(err)
	}
	if Load() != nil {
		t.Fatal("null should clear to nil")
	}
}

func TestBasicAuth(t *testing.T) {
	cfg := `{"basic":{"user":"admin","passwordBcrypt":"` + hash(t, "s3cret-pw") + `"}}`
	if err := Apply([]byte(cfg)); err != nil {
		t.Fatal(err)
	}
	c := Load()

	// missing creds → 401 + WWW-Authenticate
	ok, status, www := c.Enforce(req(nil))
	if ok || status != http.StatusUnauthorized || www == "" {
		t.Fatalf("missing creds: ok=%v status=%d www=%q", ok, status, www)
	}
	// wrong password → 401
	if ok, st, _ := c.Enforce(req(map[string]string{"Authorization": basicHeader("admin", "nope")})); ok || st != 401 {
		t.Fatalf("wrong pw should 401, got ok=%v st=%d", ok, st)
	}
	// wrong user → 401
	if ok, st, _ := c.Enforce(req(map[string]string{"Authorization": basicHeader("root", "s3cret-pw")})); ok || st != 401 {
		t.Fatalf("wrong user should 401, got ok=%v st=%d", ok, st)
	}
	// correct → allowed
	if ok, st, _ := c.Enforce(req(map[string]string{"Authorization": basicHeader("admin", "s3cret-pw")})); !ok || st != 0 {
		t.Fatalf("correct creds should pass, got ok=%v st=%d", ok, st)
	}
	// short password works (no length floor) — verify against a 1-char hash
	if err := Apply([]byte(`{"basic":{"user":"u","passwordBcrypt":"` + hash(t, "x") + `"}}`)); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := Load().Enforce(req(map[string]string{"Authorization": basicHeader("u", "x")})); !ok {
		t.Fatal("1-char password should be accepted")
	}
}

func TestIPAllowlist(t *testing.T) {
	if err := Apply([]byte(`{"ipAllowList":["203.0.113.7/32","198.51.100.0/24"]}`)); err != nil {
		t.Fatal(err)
	}
	c := Load()
	cases := []struct {
		xff  string
		want bool // allowed?
	}{
		{"203.0.113.7", true},                       // exact /32
		{"198.51.100.55", true},                     // inside /24
		{"203.0.113.8", false},                      // outside
		{"9.9.9.9", false},                          // outside
		{"203.0.113.7, 10.0.1.2", true},             // ALB appended a private hop → real client is the public-left one
		{"9.9.9.9, 203.0.113.7, 10.0.1.2", true},    // spoofed-left ignored; rightmost public = allowed client
		{"9.9.9.9, 198.51.100.5, 10.0.0.9", true},   // rightmost public (198.51.100.5) is in /24
		{"203.0.113.7, 9.9.9.9, 172.16.0.1", false}, // rightmost public is 9.9.9.9 (the real client) → blocked
	}
	for _, tc := range cases {
		ok, status, _ := c.Enforce(req(map[string]string{"X-Forwarded-For": tc.xff}))
		if ok != tc.want {
			t.Errorf("XFF %q: allowed=%v want=%v (status=%d)", tc.xff, ok, tc.want, status)
		}
		if !ok && status != http.StatusForbidden {
			t.Errorf("XFF %q: blocked should be 403, got %d", tc.xff, status)
		}
	}
}

func TestIPGateBeforeCredGate(t *testing.T) {
	// IP gate + basic both on. A blocked IP gets 403 (not 401) — we don't hint
	// that creds exist to an off-allowlist client.
	cfg := `{"basic":{"user":"a","passwordBcrypt":"` + hash(t, "pw") + `"},"ipAllowList":["203.0.113.7/32"]}`
	if err := Apply([]byte(cfg)); err != nil {
		t.Fatal(err)
	}
	c := Load()
	// off-allowlist + correct creds → still 403
	h := map[string]string{"X-Forwarded-For": "9.9.9.9", "Authorization": basicHeader("a", "pw")}
	if ok, st, _ := c.Enforce(req(h)); ok || st != http.StatusForbidden {
		t.Fatalf("blocked IP must 403 even with creds, got ok=%v st=%d", ok, st)
	}
	// on-allowlist + correct creds → allowed
	h = map[string]string{"X-Forwarded-For": "203.0.113.7", "Authorization": basicHeader("a", "pw")}
	if ok, _, _ := c.Enforce(req(h)); !ok {
		t.Fatal("on-allowlist + creds should pass")
	}
	// on-allowlist + NO creds → 401
	if ok, st, _ := c.Enforce(req(map[string]string{"X-Forwarded-For": "203.0.113.7"})); ok || st != 401 {
		t.Fatalf("on-allowlist + no creds should 401, got ok=%v st=%d", ok, st)
	}
}

func TestTokenAuth(t *testing.T) {
	// sha256("my-token") = computed; store its hex.
	// echo -n my-token | sha256sum
	const token = "my-token"
	got := sha256Hex(token)
	if err := Apply([]byte(`{"token":{"sha256":"` + got + `"}}`)); err != nil {
		t.Fatal(err)
	}
	c := Load()
	if ok, _, _ := c.Enforce(req(map[string]string{"Authorization": "Bearer " + token})); !ok {
		t.Fatal("correct bearer token should pass")
	}
	if ok, _, _ := c.Enforce(req(map[string]string{"X-Webhook-Token": token})); !ok {
		t.Fatal("correct X-Webhook-Token should pass")
	}
	if ok, st, _ := c.Enforce(req(map[string]string{"Authorization": "Bearer wrong"})); ok || st != 401 {
		t.Fatalf("wrong token should 401, got ok=%v st=%d", ok, st)
	}
	if ok, st, _ := c.Enforce(req(nil)); ok || st != 401 {
		t.Fatalf("missing token should 401, got ok=%v st=%d", ok, st)
	}
}

func TestBareIPInAllowlist(t *testing.T) {
	if err := Apply([]byte(`{"ipAllowList":["203.0.113.7"]}`)); err != nil { // bare IP → /32
		t.Fatal(err)
	}
	if ok, _, _ := Load().Enforce(req(map[string]string{"X-Forwarded-For": "203.0.113.7"})); !ok {
		t.Fatal("bare IP allowlist should match exact")
	}
	if ok, _, _ := Load().Enforce(req(map[string]string{"X-Forwarded-For": "203.0.113.8"})); ok {
		t.Fatal("bare IP allowlist should not match a different IP")
	}
}
