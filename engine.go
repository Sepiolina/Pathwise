package main

import (
	"os"
	"strings"
	"time"
)

// ---------------------------------------------------------------- engine

func Optimize(raw string, opts Options, scope Scope) *Report {
	rep := &Report{
		Scope:       scope,
		GeneratedAt: time.Now().Format(time.RFC3339),
		Original:    raw,
		OriginalLen: len(raw),
		Budget:      opts.Budget,
		Groups:      map[string]string{},
	}

	for _, seg := range strings.Split(raw, sep) {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		rep.Entries = append(rep.Entries, &Entry{Original: seg, Value: seg})
	}

	stage := func(name, detail string, fn func() int) {
		before := len(buildPath(rep.Entries, rep.GroupOrder))
		touched := fn()
		after := len(buildPath(rep.Entries, rep.GroupOrder))
		if touched > 0 || before != after {
			rep.Stages = append(rep.Stages, Stage{name, detail, before, after, touched})
		}
	}

	// 1 — trim quotes/whitespace, collapse slashes, drop trailing separators.
	if opts.Normalize {
		stage("Normalize", "trimmed quotes, slashes & trailing separators", func() int {
			n := 0
			for _, e := range rep.Entries {
				if v := normalizePath(e.Value); v != e.Value {
					e.Value = v
					n++
				}
			}
			return n
		})
	}

	// 2 — case-insensitive dedup, first occurrence wins (PATH order matters).
	if opts.Dedup {
		stage("Deduplicate", "removed repeated directories", func() int {
			seen := map[string]bool{}
			n := 0
			for _, e := range rep.Entries {
				k := dedupKey(e.Value)
				if seen[k] {
					e.Dropped, e.Reason = true, "duplicate"
					n++
					continue
				}
				seen[k] = true
			}
			return n
		})
	}

	// 3 — prune directories that no longer exist on disk.
	if opts.Prune {
		stage("Prune dead paths", "removed directories that no longer exist", func() int {
			n := 0
			for _, e := range rep.Entries {
				if e.Dropped {
					continue
				}
				expanded, resolved := expandWinChecked(e.Value)
				if !resolved {
					// e.Value references a %VARIABLE% this process doesn't
					// have set (e.g. a %PATHS_*% group variable written by
					// a previous apply, not yet visible until the terminal
					// is reopened). We can't tell if the real target exists
					// -- same principle as a permission error: skip it,
					// never guess. Leave Checked false: we never actually
					// verified this one.
					continue
				}
				e.Checked = true
				fi, err := os.Stat(expanded)
				e.Exists = err == nil && fi.IsDir()
				if os.IsNotExist(err) { // permission errors -> keep, don't guess
					e.Dropped, e.Reason = true, "not found"
					n++
				}
			}
			return n
		})
	}

	// 4 — replace literal prefixes with env tokens, only when it actually helps.
	if opts.Tokenize {
		toks := detectTokens()
		stage("Tokenize", "swapped literal prefixes for %VARIABLES%", func() int {
			n := 0
			for _, e := range rep.Entries {
				if e.Dropped {
					continue
				}
				for _, tk := range toks {
					if tk.UserOnly && scope == ScopeMachine {
						continue // machine PATH cannot expand user variables
					}
					if !hasPathPrefix(e.Value, tk.Value) {
						continue
					}
					cand := "%" + tk.Name + "%" + e.Value[len(tk.Value):]
					if len(cand) < len(e.Value) || opts.AllowGrowth {
						e.Value = cand
						n++
					}
					break
				}
			}
			return n
		})
	}

	// 5 — hoist noisy toolchains into their own variables.
	if opts.Group {
		for _, rule := range defaultGroups {
			rule := rule
			var members []string
			stage("Group "+rule.Var, "moved matching entries out of PATH", func() int {
				for _, e := range rep.Entries {
					if e.Dropped || e.Group != "" {
						continue
					}
					low := strings.ToLower(e.Value)
					for _, kw := range rule.Keywords {
						if strings.Contains(low, kw) {
							e.Group = rule.Var
							members = append(members, e.Value)
							break
						}
					}
				}
				if len(members) < 2 { // one entry isn't worth a variable
					for _, e := range rep.Entries {
						if e.Group == rule.Var {
							e.Group = ""
						}
					}
					return 0
				}
				rep.Groups[rule.Var] = strings.Join(members, sep)
				rep.GroupOrder = append(rep.GroupOrder, rule.Var)
				return len(members)
			})
		}
	}

	rep.Optimized = buildPath(rep.Entries, rep.GroupOrder)
	rep.OptimizedLen = len(rep.Optimized)
	return rep
}
