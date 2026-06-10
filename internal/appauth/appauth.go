// Package appauth holds the LIVE, hot-swappable access-control config for the
// app the daemon fronts (HTTP Basic auth, an IP allowlist, and/or a bearer
// token). It lives in its own package so both the reverse-proxy (internal/api/
// mcp) and the register-port client (internal/zibby) can use it without an
// import cycle.
//
// WHY THIS EXISTS: access control used to be baked into the ECS task definition
// (a Caddy sidecar configured via env vars). Changing it meant re-registering
// the task def and ROLLING the whole app — a multi-minute restart for heavy
// apps. Now the daemon enforces it in-process and the config is hot-swapped
// (atomic) with ZERO restart. The control plane delivers it two ways:
//   1. in the register-port response on every boot (so a restart re-applies it)
//   2. pushed live to POST /_zibby_ops/auth when the user changes it
package appauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"
)

// Config is the resolved access-control config. A nil *Config (the zero state)
// means "no gate — pass everything through". Immutable once built; swaps are
// whole-pointer (atomic) so a request either sees the old or the new config,
// never a half-applied one.
type Config struct {
	basic       *basicCred   // nil = no Basic gate
	tokenSHA256 string       // "" = no bearer-token gate
	ipNets      []*net.IPNet // empty = no IP gate
}

type basicCred struct {
	user     string
	passHash string // bcrypt hash
}

// wire is the JSON shape the control plane sends (register-port response's
// `auth` field, and the POST /_zibby_ops/auth body). NEVER carries a plaintext
// password — Basic ships the bcrypt hash, token ships a sha256 hex digest.
type wire struct {
	Basic *struct {
		User           string `json:"user"`
		PasswordBcrypt string `json:"passwordBcrypt"`
	} `json:"basic"`
	Token *struct {
		SHA256 string `json:"sha256"`
	} `json:"token"`
	IPAllowList []string `json:"ipAllowList"`
}

// current holds the live *Config. atomic.Pointer gives lock-free reads on the
// hot path (every proxied request) and a single-store swap on change.
var current atomic.Pointer[Config]

// Load returns the live config (nil when no gate is set).
func Load() *Config { return current.Load() }

// Apply parses the wire JSON and atomically swaps in the new config. An empty
// object / JSON null clears all gates (passthrough). Returns an error only on
// malformed JSON — unparseable CIDRs are skipped, not fatal.
func Apply(raw []byte) error {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		current.Store(nil)
		return nil
	}
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		return err
	}
	c := &Config{}
	if w.Basic != nil && w.Basic.User != "" && w.Basic.PasswordBcrypt != "" {
		c.basic = &basicCred{user: w.Basic.User, passHash: w.Basic.PasswordBcrypt}
	}
	if w.Token != nil && w.Token.SHA256 != "" {
		c.tokenSHA256 = strings.ToLower(strings.TrimSpace(w.Token.SHA256))
	}
	for _, raw := range w.IPAllowList {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") { // bare IP → host route
			if strings.Contains(s, ":") {
				s += "/128"
			} else {
				s += "/32"
			}
		}
		if _, n, err := net.ParseCIDR(s); err == nil {
			c.ipNets = append(c.ipNets, n)
		}
	}
	// A config with NO gate set is equivalent to passthrough — store nil so the
	// hot path can skip with a single nil check.
	if c.basic == nil && c.tokenSHA256 == "" && len(c.ipNets) == 0 {
		current.Store(nil)
		return nil
	}
	current.Store(c)
	return nil
}

// Enforce checks the request against the live config. Returns (true, 0, "")
// when allowed; otherwise (false, status, wwwAuthenticate). The IP gate is
// evaluated FIRST (403 — don't even hint that creds exist to a blocked IP),
// then the credential gate (401 + WWW-Authenticate). A nil *Config allows all.
func (c *Config) Enforce(r *http.Request) (bool, int, string) {
	if c == nil {
		return true, 0, ""
	}
	// 1. IP allowlist (independent of credentials).
	if len(c.ipNets) > 0 {
		ip := RealClientIP(r)
		ok := false
		if ip != nil {
			for _, n := range c.ipNets {
				if n.Contains(ip) {
					ok = true
					break
				}
			}
		}
		if !ok {
			return false, http.StatusForbidden, ""
		}
	}
	// 2. HTTP Basic (constant-time username + bcrypt password).
	if c.basic != nil {
		user, pass, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(c.basic.user)) != 1 ||
			bcrypt.CompareHashAndPassword([]byte(c.basic.passHash), []byte(pass)) != nil {
			return false, http.StatusUnauthorized, `Basic realm="Restricted"`
		}
	}
	// 3. Bearer / X-Webhook-Token (sha256, constant-time).
	if c.tokenSHA256 != "" {
		tok := bearerOrWebhookToken(r)
		sum := sha256.Sum256([]byte(tok))
		got := hex.EncodeToString(sum[:])
		if tok == "" || subtle.ConstantTimeCompare([]byte(got), []byte(c.tokenSHA256)) != 1 {
			return false, http.StatusUnauthorized, ""
		}
	}
	return true, 0, ""
}

// RealClientIP extracts the genuine client IP from X-Forwarded-For. The fleet
// ALB APPENDS the real connecting IP to XFF, so the trustworthy hop is the
// RIGHTMOST one — walking right-to-left and skipping private/loopback/link-
// local hops (the VPC + ALB) yields the real public client. Any client-supplied
// XFF entries sit to the LEFT of the ALB-appended IP and are never trusted
// (this is exactly what Caddy's client_ip matcher did with trusted_proxies set
// to the VPC range). Falls back to the rightmost parseable entry, then to the
// direct RemoteAddr.
func RealClientIP(r *http.Request) net.IP {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip == nil {
				continue
			}
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				continue // VPC / ALB hop — keep walking toward the client
			}
			return ip
		}
		for i := len(parts) - 1; i >= 0; i-- { // all private → rightmost wins
			if ip := net.ParseIP(strings.TrimSpace(parts[i])); ip != nil {
				return ip
			}
		}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return net.ParseIP(host)
}

func bearerOrWebhookToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[len("Bearer "):])
	}
	return strings.TrimSpace(r.Header.Get("X-Webhook-Token"))
}
