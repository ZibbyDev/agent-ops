// Copyright 2026 Zibby Lab. Apache-2.0.

// nat.go — turn the proxy box into a NAT gateway for a private subnet.
//
// A shared-ingress proxy already sits in a PUBLIC subnet with a public IP (it
// terminates TLS for the world). With a few kernel + firewall knobs it can
// ALSO masquerade egress traffic from a sibling PRIVATE subnet, so private
// backends get internet access (apt / git bootstrap) WITHOUT a separate
// managed NAT gateway. This is a standard "router on a stick" / "NAT
// instance" pattern; nothing here is cloud- or Zibby-specific.
//
// Two knobs:
//  1. net.ipv4.ip_forward=1 — let the kernel route between interfaces.
//  2. an iptables POSTROUTING MASQUERADE rule on the EGRESS interface (the one
//     holding the default route) — rewrite the private subnet's source IP to
//     the proxy's so return traffic comes back to us.
//
// Both are IDEMPOTENT: re-running `proxy up` re-asserts the sysctl (a write,
// not an append) and only inserts the iptables rule if an identical one isn't
// already present (-C check before -A). The egress interface is detected
// dynamically from the default route, so this works on any single-public-NIC
// host regardless of the interface name (eth0 / ens5 / enX0 / ...).
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// natRunner runs a command and returns combined output + error. Swappable in
// tests so we can assert the exact argv sequence without touching the host.
type natRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// natCmdRunner is the package-level runner; production uses execRunner.
var natCmdRunner natRunner = execRunner

// EnableNAT makes this host masquerade egress for traffic it forwards:
//  1. enables IPv4 forwarding (live sysctl + persisted drop-in so it survives
//     reboot),
//  2. detects the egress interface (default-route device),
//  3. ensures a single POSTROUTING MASQUERADE rule on it (idempotent).
//
// Best-effort + loud: every step logs to the writer; a failure on one step
// does not abort the others (a box that can't persist the sysctl can still
// forward for its current boot). Returns the first error encountered (for
// tests / callers that care), having attempted all steps.
func EnableNAT(ctx context.Context, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// 1. IPv4 forwarding — live + persisted.
	if err := enableIPForward(ctx, logf); err != nil {
		logf("nat: enable ip_forward: %v", err)
		note(err)
	}

	// 2. Egress interface (default-route dev).
	iface, err := defaultRouteIface(ctx)
	if err != nil || iface == "" {
		logf("nat: could not detect egress interface (default route): %v", err)
		note(fmt.Errorf("detect egress interface: %w", err))
		return firstErr
	}
	logf("nat: egress interface = %s", iface)

	// 3. MASQUERADE rule on the egress interface (idempotent).
	if err := ensureMasquerade(ctx, iface, logf); err != nil {
		logf("nat: ensure MASQUERADE on %s: %v", iface, err)
		note(err)
	}
	return firstErr
}

// ipForwardDropIn is the sysctl drop-in path. A package var (not const) so
// tests can redirect it into a temp dir via setIPForwardDropInPath.
var ipForwardDropIn = "/etc/sysctl.d/99-agent-ops-nat.conf"

func ipForwardDropInPath() string     { return ipForwardDropIn }
func setIPForwardDropInPath(p string) { ipForwardDropIn = p }

// enableIPForward sets net.ipv4.ip_forward=1 for the running kernel and writes
// a sysctl drop-in so it persists across reboot. The live `sysctl -w` is a
// last-writer-wins assignment (idempotent); the drop-in is written with fixed
// content (no append) so repeated runs don't duplicate the line.
func enableIPForward(ctx context.Context, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if out, err := natCmdRunner(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("sysctl -w net.ipv4.ip_forward=1: %w: %s", err, strings.TrimSpace(string(out)))
	}
	logf("nat: net.ipv4.ip_forward=1 (live)")
	// Persist. Fixed content => idempotent (overwrite, never append). Skip the
	// rewrite when it's already exactly right (avoids needless disk churn).
	want := "# Managed by agent-ops (proxy NAT). Do not edit.\nnet.ipv4.ip_forward=1\n"
	if cur, _ := os.ReadFile(ipForwardDropIn); string(cur) != want {
		if err := os.WriteFile(ipForwardDropIn, []byte(want), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", ipForwardDropIn, err)
		}
		logf("nat: persisted ip_forward in %s", ipForwardDropIn)
	}
	return nil
}

// defaultRouteIface returns the interface holding the IPv4 default route — the
// box's egress NIC. Parses `ip route show default`, whose first line looks
// like: `default via 10.0.0.1 dev eth0 ...`. Interface-name agnostic.
func defaultRouteIface(ctx context.Context) (string, error) {
	out, err := natCmdRunner(ctx, "ip", "route", "show", "default")
	if err != nil {
		return "", fmt.Errorf("ip route show default: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return parseDefaultRouteIface(out)
}

// parseDefaultRouteIface extracts the `dev <iface>` token from `ip route`
// output. Split out for unit testing without a host route table.
func parseDefaultRouteIface(out []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		// Only consider DEFAULT routes (some callers pass `ip route` for the
		// whole table; a `10.0.0.0/24 dev eth0` line is NOT our egress route).
		if len(fields) == 0 || fields[0] != "default" {
			continue
		}
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "dev" {
				return fields[i+1], nil
			}
		}
	}
	return "", fmt.Errorf("no `dev <iface>` token in default route")
}

// ensureMasquerade inserts a POSTROUTING MASQUERADE rule on iface only if an
// identical rule isn't already present. iptables `-C` (check) exits 0 when the
// rule exists, non-zero when it doesn't — so we append (`-A`) only on the
// not-present case. Re-running never duplicates the rule.
func ensureMasquerade(ctx context.Context, iface string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// `-t nat` MUST precede the -C/-A chain operation; iptables expects the
	// chain name immediately after -C/-A. (Building `-A -t nat POSTROUTING`
	// makes iptables choke with "Bad argument `nat'".)
	rule := []string{"POSTROUTING", "-o", iface, "-j", "MASQUERADE"}
	// -C: does the rule already exist?
	if _, err := natCmdRunner(ctx, "iptables", append([]string{"-t", "nat", "-C"}, rule...)...); err == nil {
		logf("nat: MASQUERADE rule already present on %s (idempotent no-op)", iface)
		return nil
	}
	if out, err := natCmdRunner(ctx, "iptables", append([]string{"-t", "nat", "-A"}, rule...)...); err != nil {
		return fmt.Errorf("iptables -A POSTROUTING MASQUERADE: %w: %s", err, strings.TrimSpace(string(out)))
	}
	logf("nat: added MASQUERADE rule on %s", iface)
	return nil
}
