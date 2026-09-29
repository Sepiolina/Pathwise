package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// noopOptions runs the pipeline with every stage disabled, so Optimize just
// splits the raw PATH into entries without changing anything.
func noopOptions() Options { return Options{Budget: defaultBudget} }

func TestOptimize_SplitsAndRebuildsUnchanged(t *testing.T) {
	raw := `C:\A` + sep + `C:\B` + sep + `C:\C`
	rep := Optimize(raw, noopOptions(), ScopeUser)

	if len(rep.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(rep.Entries))
	}
	if rep.Optimized != raw {
		t.Errorf("Optimized = %q, want unchanged %q", rep.Optimized, raw)
	}
	if len(rep.Stages) != 0 {
		t.Errorf("expected no stages to run, got %v", rep.Stages)
	}
}

func TestOptimize_SkipsBlankSegments(t *testing.T) {
	raw := `C:\A;;  ;C:\B;`
	rep := Optimize(raw, noopOptions(), ScopeUser)
	if len(rep.Entries) != 2 {
		t.Fatalf("got %d entries, want 2 (blank segments dropped): %+v", len(rep.Entries), rep.Entries)
	}
}

func TestOptimize_Normalize(t *testing.T) {
	raw := `  "C:\Program Files\Git\cmd"  ` + sep + `C:\Windows\`
	opts := Options{Normalize: true, Budget: defaultBudget}
	rep := Optimize(raw, opts, ScopeUser)

	want := `C:\Program Files\Git\cmd` + sep + `C:\Windows`
	if rep.Optimized != want {
		t.Errorf("Optimized = %q, want %q", rep.Optimized, want)
	}
	if len(rep.Stages) != 1 || rep.Stages[0].Name != "Normalize" {
		t.Errorf("expected a single Normalize stage, got %+v", rep.Stages)
	}
}

func TestOptimize_DeduplicateIsCaseInsensitiveAndKeepsFirst(t *testing.T) {
	raw := `C:\Windows\System32` + sep + `C:\windows\system32` + sep + `C:\Tools`
	opts := Options{Dedup: true, Budget: defaultBudget}
	rep := Optimize(raw, opts, ScopeUser)

	want := `C:\Windows\System32` + sep + `C:\Tools`
	if rep.Optimized != want {
		t.Errorf("Optimized = %q, want %q", rep.Optimized, want)
	}

	var dropped int
	for _, e := range rep.Entries {
		if e.Dropped {
			dropped++
			if e.Reason != "duplicate" {
				t.Errorf("dropped entry reason = %q, want %q", e.Reason, "duplicate")
			}
		}
	}
	if dropped != 1 {
		t.Errorf("expected exactly 1 duplicate dropped, got %d", dropped)
	}
}

func TestOptimize_PrunesDeadDirectoriesOnly(t *testing.T) {
	live := t.TempDir()
	dead := filepath.Join(live, "does-not-exist")

	raw := live + sep + dead
	opts := Options{Prune: true, Budget: defaultBudget}
	rep := Optimize(raw, opts, ScopeUser)

	if rep.Optimized != live {
		t.Errorf("Optimized = %q, want just the live dir %q", rep.Optimized, live)
	}
	if len(rep.Stages) != 1 || rep.Stages[0].Touched != 1 {
		t.Errorf("expected exactly 1 pruned entry, got stages %+v", rep.Stages)
	}
	for _, e := range rep.Entries {
		if e.Original == live && (e.Dropped || !e.Exists) {
			t.Errorf("live directory should not be dropped: %+v", e)
		}
		if e.Original == dead && (!e.Dropped || e.Reason != "not found") {
			t.Errorf("dead directory should be dropped as not found: %+v", e)
		}
	}
}

// Regression test: an entry referencing a %VARIABLE% this process doesn't
// have set (e.g. a %PATHS_JAVA% group variable pathwise itself wrote to the
// registry on a previous run, not yet visible until the terminal is
// reopened) must be left alone by Prune, not dropped as "not found". Prune
// can only judge paths it can actually resolve.
func TestOptimize_PruneSkipsUnresolvableTokens(t *testing.T) {
	raw := `%PATHS_JAVA%` + sep + `C:\Windows`
	opts := Options{Prune: true, Budget: defaultBudget}
	rep := Optimize(raw, opts, ScopeUser)

	for _, e := range rep.Entries {
		if e.Original == `%PATHS_JAVA%` {
			if e.Dropped {
				t.Errorf("unresolvable token entry must not be dropped, got reason %q", e.Reason)
			}
			if e.Checked {
				t.Errorf("unresolvable token entry was never actually verified, Checked should be false")
			}
		}
	}
}

// Sharper version of the same regression: the target directory genuinely
// exists, but is only reachable through a token this process can't resolve.
// It must survive Prune -- dropping it here would be indistinguishable from
// dropping a real dead directory, which is exactly the bug: silently
// breaking a valid %PATHS_*% reference right after the tool created it.
func TestOptimize_PruneKeepsRealDirectoryBehindUnresolvedToken(t *testing.T) {
	live := t.TempDir()
	t.Setenv("PATHWISE_RESOLVABLE", live) // resolvable control case

	raw := `%PATHWISE_RESOLVABLE%` + sep + `%PATHWISE_UNRESOLVABLE%`
	opts := Options{Prune: true, Budget: defaultBudget}
	rep := Optimize(raw, opts, ScopeUser)

	for _, e := range rep.Entries {
		switch e.Original {
		case `%PATHWISE_RESOLVABLE%`:
			if e.Dropped || !e.Checked || !e.Exists {
				t.Errorf("resolvable token pointing at a real directory should survive and be marked existing: %+v", e)
			}
		case `%PATHWISE_UNRESOLVABLE%`:
			if e.Dropped {
				t.Errorf("unresolvable token must not be dropped -- we have no way to know if its target exists: %+v", e)
			}
		}
	}
}

func TestOptimize_PruneDisabledKeepsDeadEntries(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "does-not-exist")
	rep := Optimize(dead, Options{Budget: defaultBudget}, ScopeUser)
	if rep.Optimized != dead {
		t.Errorf("Optimized = %q, want unchanged %q (prune disabled)", rep.Optimized, dead)
	}
}

func TestOptimize_TokenizeOnlyWhenShorter(t *testing.T) {
	t.Setenv("ProgramFiles", `C:\Program Files`)

	opts := Options{Tokenize: true, Budget: defaultBudget}
	rep := Optimize(`C:\Program Files\Git\cmd`, opts, ScopeUser)

	want := `%ProgramFiles%\Git\cmd`
	if rep.Optimized != want {
		t.Errorf("Optimized = %q, want %q", rep.Optimized, want)
	}
}

func TestOptimize_TokenizeSkipsUserVarsOnMachineScope(t *testing.T) {
	t.Setenv("USERPROFILE", `C:\Users\Dev`)

	opts := Options{Tokenize: true, Budget: defaultBudget}
	rep := Optimize(`C:\Users\Dev\AppData\Local\Bin`, opts, ScopeMachine)

	if strings.Contains(rep.Optimized, "%") {
		t.Errorf("machine scope should never get a user-only token, got %q", rep.Optimized)
	}
}

func TestOptimize_GroupsToolchainsWhenTwoOrMoreMatch(t *testing.T) {
	raw := strings.Join([]string{
		`C:\Program Files\nodejs`,
		`C:\Users\Dev\AppData\Roaming\npm`,
		`C:\Tools\Unrelated`,
	}, sep)
	opts := Options{Group: true, Budget: defaultBudget}
	rep := Optimize(raw, opts, ScopeUser)

	if !strings.Contains(rep.Optimized, "%PATHS_NODE%") {
		t.Errorf("expected PATHS_NODE variable in optimized PATH, got %q", rep.Optimized)
	}
	if !strings.Contains(rep.Optimized, `C:\Tools\Unrelated`) {
		t.Errorf("unrelated entry should stay in PATH, got %q", rep.Optimized)
	}
	if got := rep.Groups["PATHS_NODE"]; got == "" {
		t.Errorf("expected PATHS_NODE group contents to be recorded")
	}
}

func TestOptimize_SingleMatchIsNotWorthAGroup(t *testing.T) {
	raw := `C:\Program Files\nodejs` + sep + `C:\Tools\Unrelated`
	opts := Options{Group: true, Budget: defaultBudget}
	rep := Optimize(raw, opts, ScopeUser)

	if len(rep.Groups) != 0 {
		t.Errorf("a single matching entry should not become a group, got %+v", rep.Groups)
	}
	if rep.Optimized != raw {
		t.Errorf("Optimized = %q, want unchanged %q", rep.Optimized, raw)
	}
}

func TestOptimize_SavedReflectsLengthDelta(t *testing.T) {
	raw := `C:\Windows\System32` + sep + `C:\windows\system32`
	opts := Options{Dedup: true, Budget: defaultBudget}
	rep := Optimize(raw, opts, ScopeUser)

	if rep.Saved() != rep.OriginalLen-rep.OptimizedLen {
		t.Errorf("Saved() = %d, want %d", rep.Saved(), rep.OriginalLen-rep.OptimizedLen)
	}
	if rep.Saved() <= 0 {
		t.Errorf("expected deduplication to save bytes, got Saved() = %d", rep.Saved())
	}
}
