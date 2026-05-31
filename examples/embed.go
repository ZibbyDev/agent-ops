// Copyright 2026 Zibby Lab. Apache-2.0.

// Package examples embeds the bundled config.yaml templates that ship
// inside the agent-ops binary.
//
// The same YAML files are also kept on disk at the repo root so they're
// browsable on GitHub — the embed is the binary-side mirror so users can
// `agent-ops init --template <name>` (or call the daemon's MCP
// `agent_apply_template` tool) without a network round-trip.
//
// Add a new template by dropping it next to this file with a leading `#`
// comment as its one-line description. List() picks it up automatically.
package examples

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

// templatesFS is the embedded set of YAML templates that ship with the
// binary. Filenames map 1:1 to the on-disk names in this directory, minus
// the `.yaml` suffix (e.g. `wordpress-multisite.yaml` → "wordpress-multisite").
//
//go:embed *.yaml
var templatesFS embed.FS

// Template is one embedded config template's metadata.
type Template struct {
	// Name is the filename stem (e.g. "wordpress-multisite"). It's what
	// users pass to `agent-ops init --template <name>` and to the
	// `agent_apply_template` MCP tool.
	Name string
	// Description is parsed from the first `#`-prefixed comment line of
	// the YAML — typically a one-line summary of what the template covers.
	// Falls back to Name when the file has no leading comment.
	Description string
	// Filename is the basename including extension. Always `<Name>.yaml`.
	Filename string
}

// List returns every embedded template, sorted by Name.
//
// Errors from the embedded FS are unreachable under normal conditions
// (go:embed bakes the data in at build time), so we don't surface them —
// any disagreement between the directives and the FS is a build-time bug
// that would have failed `go build` already.
func List() []Template {
	entries, err := templatesFS.ReadDir(".")
	if err != nil {
		// embed.FS.ReadDir(".") on a baked FS doesn't fail in practice;
		// returning empty here lets callers degrade gracefully.
		return nil
	}
	out := make([]Template, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		body, err := templatesFS.ReadFile(name)
		if err != nil {
			continue
		}
		stem := strings.TrimSuffix(name, ".yaml")
		out = append(out, Template{
			Name:        stem,
			Description: firstCommentLine(body, stem),
			Filename:    name,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns just the sorted template names — handy for error messages
// where we want to suggest "Available templates: a, b, c".
func Names() []string {
	all := List()
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.Name)
	}
	return out
}

// Get returns the raw YAML bytes for the named template (no `.yaml`
// suffix). Returns a wrapped error listing the available names when the
// template isn't found, so the CLI / MCP tool can pass it straight to the
// user without further formatting.
func Get(name string) ([]byte, error) {
	body, err := templatesFS.ReadFile(name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("template %q not found; available: %s",
			name, strings.Join(Names(), ", "))
	}
	return body, nil
}

// firstCommentLine extracts the first `#`-prefixed line from the YAML
// body, stripping the `#` and a single leading space. Used as the
// template's user-facing description. Falls back to fallback when no
// comment is present (or it's empty after trimming).
func firstCommentLine(body []byte, fallback string) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "#"))
		// Common pattern in our examples is `# Example config — <desc>.`
		// — we want the part AFTER the `—` if present, otherwise the
		// whole line. The em-dash style is a convention the templates
		// follow; non-conforming files fall through to the raw comment.
		if i := strings.Index(trimmed, "—"); i >= 0 {
			rest := strings.TrimSpace(trimmed[i+len("—"):])
			rest = strings.TrimSuffix(rest, ".")
			if rest != "" {
				return rest
			}
		}
		if trimmed != "" {
			return trimmed
		}
		break
	}
	return fallback
}
