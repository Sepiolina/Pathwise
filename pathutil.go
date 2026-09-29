package main

import (
	"os"
	"strings"
)

// ---------------------------------------------------------------- helpers

// hasPathPrefix is boundary-aware: C:\Windows does NOT match C:\WindowsApps.
func hasPathPrefix(s, prefix string) bool {
	if prefix == "" || len(s) < len(prefix) {
		return false
	}
	if !strings.EqualFold(s[:len(prefix)], prefix) {
		return false
	}
	if len(s) == len(prefix) {
		return true
	}
	c := s[len(prefix)]
	return c == '\\' || c == '/'
}

// expandWin substitutes %VAR% references using the current process's
// environment. A variable this process doesn't have set is left literal
// (see expandWinChecked for callers that need to know when that happens).
func expandWin(s string) string {
	out, _ := expandWinChecked(s)
	return out
}

// expandWinChecked behaves exactly like expandWin, but also reports whether
// every %VAR% reference in s was actually resolved. resolved is false when s
// contains a %NAME% token this process's environment doesn't have set --
// e.g. a %PATHS_*% group variable pathwise itself just wrote to the
// registry, which won't be visible here until the terminal is reopened.
//
// Callers that would otherwise treat "couldn't resolve" the same as
// "resolved to a nonexistent path" (most importantly the Prune stage, which
// must never guess) should use this instead of expandWin.
func expandWinChecked(s string) (string, bool) {
	var b strings.Builder
	resolved := true
	for i := 0; i < len(s); {
		if s[i] == '%' {
			if j := strings.IndexByte(s[i+1:], '%'); j > 0 {
				name := s[i+1 : i+1+j]
				if v, ok := os.LookupEnv(name); ok {
					b.WriteString(v)
					i += j + 2
					continue
				}
				resolved = false
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), resolved
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	p = strings.Trim(p, `"`)
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	unc := strings.HasPrefix(p, `\\`)
	p = strings.ReplaceAll(p, "/", `\`)
	body := p
	if unc {
		body = p[2:]
	}
	for strings.Contains(body, `\\`) {
		body = strings.ReplaceAll(body, `\\`, `\`)
	}
	if unc {
		p = `\\` + body
	} else {
		p = body
	}
	if len(p) > 3 && strings.HasSuffix(p, `\`) {
		p = strings.TrimRight(p, `\`)
	}
	return p
}

// dedupKey is the canonical form used to detect duplicate PATH entries.
// NOTE: like expandWin, this can only resolve %VAR% tokens this process has
// set. A literal path and an unresolvable-token entry pointing at the same
// real directory won't compare equal -- a missed dedup, not a destructive
// one, so it's left as a known limitation rather than guessed at.
func dedupKey(v string) string { return strings.ToLower(normalizePath(expandWin(v))) }

func buildPath(entries []*Entry, groupOrder []string) string {
	out := make([]string, 0, len(entries)+len(groupOrder))
	for _, e := range entries {
		if e.Dropped || e.Group != "" {
			continue
		}
		out = append(out, e.Value)
	}
	for _, g := range groupOrder {
		out = append(out, "%"+g+"%")
	}
	return strings.Join(out, sep)
}
