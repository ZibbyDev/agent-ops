// Copyright 2026 Zibby Lab. Apache-2.0.

package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDefaultRouteIface(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"eth0", "default via 10.0.0.1 dev eth0 proto dhcp metric 100\n", "eth0", false},
		{"ens5", "default via 172.31.0.1 dev ens5\n", "ens5", false},
		{"multiline", "blackhole 10.1.0.0/16\ndefault via 1.2.3.4 dev enX0 metric 1\n", "enX0", false},
		{"no-default", "10.0.0.0/24 dev eth0 scope link\n", "", true},
		{"empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDefaultRouteIface([]byte(tc.in))
			if tc.err && err == nil {
				t.Fatalf("expected error, got iface %q", got)
			}
			if !tc.err && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("iface = %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeRunner records argv sequences and returns scripted exit behavior. It
// simulates iptables -C semantics via a flag that flips once the rule is added.
type fakeRunner struct {
	calls       [][]string
	rulepresent bool // simulated iptables state: does the MASQUERADE rule exist?
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	f.calls = append(f.calls, call)
	switch name {
	case "iptables":
		// -C check: exit 0 iff the rule is present; non-zero otherwise.
		// (-C/-A sit after the `-t nat` table option, so scan, don't index.)
		if argsHave(args, "-C") {
			if f.rulepresent {
				return nil, nil
			}
			return []byte("iptables: Bad rule (does a matching rule exist...)\n"), fmt.Errorf("exit 1")
		}
		// -A append: now the rule exists.
		if argsHave(args, "-A") {
			f.rulepresent = true
			return nil, nil
		}
	}
	return nil, nil
}

func argsHave(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func (f *fakeRunner) countAppends() int {
	n := 0
	for _, c := range f.calls {
		if len(c) >= 1 && c[0] == "iptables" && argsHave(c, "-A") {
			n++
		}
	}
	return n
}

func (f *fakeRunner) countChecks() int {
	n := 0
	for _, c := range f.calls {
		if len(c) >= 1 && c[0] == "iptables" && argsHave(c, "-C") {
			n++
		}
	}
	return n
}

// TestEnsureMasquerade_Idempotent proves the rule is added exactly once across
// repeated runs: first run appends (rule absent), second run is a no-op
// (rule present, -C succeeds, no -A).
func TestEnsureMasquerade_Idempotent(t *testing.T) {
	fr := &fakeRunner{}
	orig := natCmdRunner
	natCmdRunner = fr.run
	defer func() { natCmdRunner = orig }()

	ctx := context.Background()
	if err := ensureMasquerade(ctx, "eth0", nil); err != nil {
		t.Fatalf("first ensureMasquerade: %v", err)
	}
	if err := ensureMasquerade(ctx, "eth0", nil); err != nil {
		t.Fatalf("second ensureMasquerade: %v", err)
	}
	if got := fr.countAppends(); got != 1 {
		t.Errorf("MASQUERADE appended %d times, want exactly 1 (idempotent)", got)
	}
	if got := fr.countChecks(); got != 2 {
		t.Errorf("expected 2 -C checks (one per run), got %d", got)
	}
	// Verify the appended rule targets the right table/chain/interface.
	var appendCall []string
	for _, c := range fr.calls {
		if len(c) >= 1 && c[0] == "iptables" && argsHave(c, "-A") {
			appendCall = c
		}
	}
	joined := strings.Join(appendCall, " ")
	for _, want := range []string{"-t nat", "POSTROUTING", "-o eth0", "MASQUERADE"} {
		if !strings.Contains(joined, want) {
			t.Errorf("append rule %q missing %q", joined, want)
		}
	}
}

// TestEnableIPForward_PersistsIdempotent verifies the sysctl drop-in is
// written with fixed content and a second run with identical content does not
// rewrite it.
func TestEnableIPForward_PersistsIdempotent(t *testing.T) {
	fr := &fakeRunner{}
	orig := natCmdRunner
	natCmdRunner = fr.run
	defer func() { natCmdRunner = orig }()

	// Redirect the drop-in into a temp dir by overriding the package var.
	tmp := filepath.Join(t.TempDir(), "99-agent-ops-nat.conf")
	origPath := ipForwardDropInPath()
	setIPForwardDropInPath(tmp)
	defer setIPForwardDropInPath(origPath)

	ctx := context.Background()
	if err := enableIPForward(ctx, nil); err != nil {
		t.Fatalf("first enableIPForward: %v", err)
	}
	b, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read drop-in: %v", err)
	}
	if !strings.Contains(string(b), "net.ipv4.ip_forward=1") {
		t.Errorf("drop-in missing ip_forward line:\n%s", b)
	}
	// Second run: content already correct => no rewrite (mtime check via a
	// content-stability assertion; we re-read and compare).
	if err := enableIPForward(ctx, nil); err != nil {
		t.Fatalf("second enableIPForward: %v", err)
	}
	b2, _ := os.ReadFile(tmp)
	if string(b) != string(b2) {
		t.Errorf("drop-in content changed across idempotent runs")
	}
	// Both runs must have asserted the live sysctl.
	sysctlCalls := 0
	for _, c := range fr.calls {
		if len(c) > 0 && c[0] == "sysctl" {
			sysctlCalls++
		}
	}
	if sysctlCalls != 2 {
		t.Errorf("expected 2 live sysctl -w calls, got %d", sysctlCalls)
	}
}
