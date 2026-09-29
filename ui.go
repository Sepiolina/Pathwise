package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------- render

type UI struct {
	t     *Theme
	w     int
	stdin *bufio.Reader
	store *Store
	demo  bool
}

type column struct {
	title string
	right bool
}

// renderTable is ANSI-aware (lipgloss.Width ignores escape codes), so styled
// cells still line up. Hand-rolled to avoid lipgloss/table version drift.
func (u *UI) renderTable(cols []column, rows [][]string) string {
	n := len(cols)
	w := make([]int, n)
	for i, c := range cols {
		w[i] = lipgloss.Width(c.title)
	}
	for _, r := range rows {
		for i := 0; i < n && i < len(r); i++ {
			if x := lipgloss.Width(r[i]); x > w[i] {
				w[i] = x
			}
		}
	}
	pad := func(s string, i int) string {
		gap := w[i] - lipgloss.Width(s)
		if gap < 0 {
			gap = 0
		}
		if cols[i].right {
			return strings.Repeat(" ", gap) + s
		}
		return s + strings.Repeat(" ", gap)
	}
	rule := func(l, m, r string) string {
		seg := make([]string, n)
		for i := range seg {
			seg[i] = strings.Repeat("─", w[i]+2)
		}
		return u.t.Border.Render(l + strings.Join(seg, m) + r)
	}
	line := func(cells []string) string {
		out := make([]string, n)
		for i := 0; i < n; i++ {
			s := ""
			if i < len(cells) {
				s = cells[i]
			}
			out[i] = " " + pad(s, i) + " "
		}
		bar := u.t.Border.Render("│")
		return bar + strings.Join(out, bar) + bar
	}

	head := make([]string, n)
	for i, c := range cols {
		head[i] = u.t.Subtitle.Render(c.title)
	}
	var b strings.Builder
	b.WriteString(rule("╭", "┬", "╮") + "\n")
	b.WriteString(line(head) + "\n")
	b.WriteString(rule("├", "┼", "┤") + "\n")
	for _, r := range rows {
		b.WriteString(line(r) + "\n")
	}
	b.WriteString(rule("╰", "┴", "╯"))
	return b.String()
}

func (u *UI) header(scope Scope, source string) {
	fmt.Println()
	fmt.Println(u.t.Title.Render(fmt.Sprintf(" %s  v%s ", strings.ToUpper(appName), appVersion)))
	fmt.Println(u.t.Muted.Render("  Windows PATH analyzer, optimizer & time machine"))
	fmt.Printf("  %s %s   %s %s\n",
		u.t.Muted.Render("scope:"), u.t.Chip.Render(string(scope)),
		u.t.Muted.Render("source:"), u.t.Path.Render(source))
	if u.store != nil {
		fmt.Printf("  %s %s\n", u.t.Muted.Render("history:"), u.t.Path.Render(u.store.File))
	} else {
		fmt.Println("  " + u.t.Warn.Render("history: disabled"))
	}
	fmt.Println()
}

func (u *UI) gauge(label string, n, budget int) {
	status, st := "HEALTHY", u.t.Success
	switch {
	case n > hardLimit:
		status, st = "EXCEEDS REGISTRY LIMIT", u.t.Danger
	case n > budget:
		status, st = "OVER BUDGET", u.t.Danger
	case float64(n) > float64(budget)*0.85:
		status, st = "APPROACHING LIMIT", u.t.Warn
	}
	fmt.Printf("  %-11s %s  %s  %s\n",
		u.t.Subtitle.Render(label),
		u.t.Bar(n, budget, 34),
		fmt.Sprintf("%5d / %d", n, budget),
		st.Render(status))
}

func (u *UI) stages(rep *Report) {
	if len(rep.Stages) == 0 {
		fmt.Println("\n  " + u.t.Muted.Render("No changes — your PATH is already clean."))
		return
	}
	rows := make([][]string, 0, len(rep.Stages))
	for _, s := range rep.Stages {
		d, sign := s.Delta(), "-"
		if d < 0 {
			d, sign = -d, "+"
		}
		delta := u.t.Muted.Render("0")
		if s.Delta() > 0 {
			delta = u.t.Success.Render(fmt.Sprintf("%s%d B", sign, d))
		} else if s.Delta() < 0 {
			delta = u.t.Warn.Render(fmt.Sprintf("%s%d B", sign, d))
		}
		rows = append(rows, []string{
			s.Name, u.t.Path.Render(s.Detail), strconv.Itoa(s.Touched), delta,
		})
	}
	fmt.Println()
	fmt.Println(u.renderTable([]column{
		{"STAGE", false}, {"WHAT IT DID", false},
		{"ITEMS", true}, {"PATH DELTA", true},
	}, rows))
}

func (u *UI) findings(rep *Report) {
	var dupes, dead []string
	for _, e := range rep.Entries {
		switch e.Reason {
		case "duplicate":
			dupes = append(dupes, e.Original)
		case "not found":
			dead = append(dead, e.Original)
		}
	}
	sort.Strings(dupes)
	sort.Strings(dead)
	show := func(icon, title string, st lipgloss.Style, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Printf("\n  %s %s\n", st.Render(icon), u.t.Subtitle.Render(
			fmt.Sprintf("%s (%d)", title, len(items))))
		for i, v := range items {
			if i == 6 {
				fmt.Println("    " + u.t.Muted.Render(
					fmt.Sprintf("… and %d more", len(items)-6)))
				break
			}
			fmt.Println("    " + u.t.Muted.Render("• ") + u.t.Path.Render(v))
		}
	}
	show("⧉", "Duplicates removed", u.t.Warn, dupes)
	show("✗", "Dead directories removed", u.t.Danger, dead)

	if len(rep.Groups) > 0 {
		fmt.Printf("\n  %s %s\n", u.t.Success.Render("◆"),
			u.t.Subtitle.Render("Variables created"))
		for _, g := range rep.GroupOrder {
			fmt.Printf("    %s %s\n",
				u.t.Chip.Render("%"+g+"%"),
				u.t.Muted.Render(fmt.Sprintf("%d B, %d entries",
					len(rep.Groups[g]), strings.Count(rep.Groups[g], sep)+1)))
		}
	}
}

func (u *UI) warnings(rep *Report) {
	var w []string
	if !rep.Scope.writable() {
		w = append(w, "Process scope is READ-ONLY here. It merges Machine+User PATH — "+
			"writing it back to either hive would corrupt your environment.")
	}
	if rep.Scope == ScopeMachine && hasUserOnlyToken(rep.Entries) {
		w = append(w, "Machine PATH cannot expand %USERPROFILE%, %APPDATA%, or "+
			"%LOCALAPPDATA%. Move those entries to User scope.")
	}
	if rep.OptimizedLen > setxLimit {
		w = append(w, fmt.Sprintf("PATH is %d B — never apply it with setx.exe "+
			"(silently truncates at %d B). Use the generated script.",
			rep.OptimizedLen, setxLimit))
	}
	if u.demo {
		w = append(w, "Demo mode: this is synthetic data. Applying is disabled.")
	}
	if len(w) == 0 {
		return
	}
	fmt.Println()
	for _, m := range w {
		fmt.Println("  " + u.t.Warn.Render("⚠ "+m))
	}
}

func (u *UI) summary(rep *Report) {
	fmt.Println()
	u.gauge("BEFORE", rep.OriginalLen, rep.Budget)
	u.gauge("AFTER ", rep.OptimizedLen, rep.Budget)
	pct := 0.0
	if rep.OriginalLen > 0 {
		pct = float64(rep.Saved()) / float64(rep.OriginalLen) * 100
	}
	fmt.Printf("\n  %s %s\n", u.t.Subtitle.Render("Net result:"),
		u.t.Success.Render(fmt.Sprintf("%d bytes reclaimed (%.1f%%)", rep.Saved(), pct)))
	if len(rep.Groups) > 0 {
		fmt.Println("  " + u.t.Muted.Render(
			"Grouped entries still exist — they just moved out of PATH into their own variables."))
	}
	u.stages(rep)
	u.findings(rep)
	u.warnings(rep)
}

func (u *UI) diff(rep *Report) {
	fmt.Println("\n  " + u.t.Subtitle.Render("BEFORE → AFTER"))
	for _, e := range rep.Entries {
		switch {
		case e.Dropped:
			fmt.Printf("   %s %s %s\n", u.t.Danger.Render("−"),
				u.t.Muted.Render(e.Original), u.t.Danger.Render("["+e.Reason+"]"))
		case e.Group != "":
			fmt.Printf("   %s %s %s\n", u.t.Info.Render("→"),
				u.t.Path.Render(e.Value), u.t.Chip.Render("%"+e.Group+"%"))
		case e.Value != e.Original:
			fmt.Printf("   %s %s\n", u.t.Success.Render("~"), u.t.Success.Render(e.Value))
			fmt.Printf("     %s\n", u.t.Muted.Render("was: "+e.Original))
		default:
			fmt.Printf("   %s %s\n", u.t.Muted.Render("="), u.t.Path.Render(e.Value))
		}
	}
}

// ---------------------------------------------------------------- history UI

func (u *UI) showHistory(scope Scope, limit int) {
	if u.store == nil {
		fmt.Println("\n  " + u.t.Warn.Render("History is disabled."))
		return
	}
	snaps, err := u.store.List(scope, limit)
	if err != nil {
		fmt.Println("  " + u.t.Danger.Render(err.Error()))
		return
	}
	fmt.Println("\n  " + u.t.Subtitle.Render("SNAPSHOTS"))
	if len(snaps) == 0 {
		fmt.Println("  " + u.t.Muted.Render("Nothing recorded yet."))
	} else {
		rows := make([][]string, 0, len(snaps))
		for _, s := range snaps {
			kind := u.t.Muted.Render(s.Kind)
			if s.Kind != "auto" {
				kind = u.t.Info.Render(s.Kind)
			}
			reg := u.t.Muted.Render("—")
			if s.RegFile != "" {
				reg = u.t.Success.Render("✔")
			}
			rows = append(rows, []string{
				strconv.FormatInt(s.ID, 10),
				s.TakenAt.Local().Format("2006-01-02 15:04"),
				string(s.Scope), kind,
				strconv.Itoa(s.Length),
				strconv.Itoa(len(s.Vars)), reg,
			})
		}
		fmt.Println(u.renderTable([]column{
			{"ID", true}, {"TAKEN", false}, {"SCOPE", false},
			{"KIND", false}, {"BYTES", true}, {"VARS", true}, {".REG", false},
		}, rows))
	}

	runs, err := u.store.Runs(limit)
	if err != nil || len(runs) == 0 {
		return
	}
	fmt.Println("\n  " + u.t.Subtitle.Render("RUNS"))
	rows := make([][]string, 0, len(runs))
	for _, r := range runs {
		applied := u.t.Muted.Render("analyzed")
		if r.Applied {
			applied = u.t.Success.Render("applied")
		}
		rows = append(rows, []string{
			strconv.FormatInt(r.ID, 10),
			r.RanAt.Local().Format("2006-01-02 15:04"),
			string(r.Scope),
			strconv.Itoa(r.BeforeLen), strconv.Itoa(r.AfterLen),
			strconv.Itoa(r.BeforeLen - r.AfterLen), applied,
		})
	}
	fmt.Println(u.renderTable([]column{
		{"ID", true}, {"RAN", false}, {"SCOPE", false},
		{"BEFORE", true}, {"AFTER", true}, {"SAVED", true}, {"RESULT", false},
	}, rows))
}

func (u *UI) restore(id int64, scope Scope) bool {
	if u.store == nil {
		fmt.Println("\n  " + u.t.Warn.Render("History is disabled — nothing to restore from."))
		return false
	}
	if runtime.GOOS != "windows" {
		fmt.Println("\n  " + u.t.Warn.Render("Restore only works on Windows."))
		return false
	}
	if id == 0 { // interactive picker
		u.showHistory(scope, 15)
		v := u.ask("\n  Snapshot ID to restore (blank = cancel): ")
		if v == "" {
			return false
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			fmt.Println("  " + u.t.Warn.Render("Not a number."))
			return false
		}
		id = n
	}
	sn, err := u.store.Get(id)
	if err != nil {
		fmt.Println("  " + u.t.Danger.Render(err.Error()))
		return false
	}
	if !sn.Scope.writable() {
		fmt.Println("  " + u.t.Danger.Render("That snapshot is process scope — not restorable."))
		return false
	}

	// Managed vars that exist now but weren't in the snapshot get removed.
	var remove []string
	if now, err := listManagedVars(sn.Scope); err == nil {
		for _, n := range now {
			if _, ok := sn.Vars[n]; !ok {
				remove = append(remove, n)
			}
		}
	}

	fmt.Println("\n" + u.t.Box.Render(
		u.t.Subtitle.Render(fmt.Sprintf("Restore snapshot #%d", sn.ID))+"\n"+
			u.t.Muted.Render("taken   ")+sn.TakenAt.Local().Format("2006-01-02 15:04:05")+"\n"+
			u.t.Muted.Render("scope   ")+string(sn.Scope)+"\n"+
			u.t.Muted.Render("length  ")+fmt.Sprintf("%d bytes, %d entries",
			sn.Length, strings.Count(sn.Path, sep)+1)+"\n"+
			u.t.Muted.Render("vars    ")+fmt.Sprintf("%d restored, %d removed",
			len(sn.Vars), len(remove))))

	if u.ask("  Type "+u.t.Chip.Render("RESTORE")+" to confirm: ") != "RESTORE" {
		fmt.Println("  " + u.t.Muted.Render("Cancelled."))
		return false
	}
	if _, err := capture(u.store, sn.Scope, "pre-restore",
		fmt.Sprintf("before restoring #%d", sn.ID)); err != nil {
		fmt.Println("  " + u.t.Warn.Render("Pre-restore snapshot failed: "+err.Error()))
	}
	if err := u.runScript(buildRestoreScript(sn, remove), sn.Scope); err != nil {
		fmt.Println("  " + u.t.Danger.Render("Restore failed: "+err.Error()))
		return false
	}
	_, _ = capture(u.store, sn.Scope, "post-restore", fmt.Sprintf("restored #%d", sn.ID))
	fmt.Println("\n  " + u.t.Success.Render("✔ Restored. Open a new terminal.\n"))
	return true
}

// ---------------------------------------------------------------- actions

func (u *UI) ask(prompt string) string {
	fmt.Print(prompt)
	line, err := u.stdin.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimSpace(line)
}

func onOff(t *Theme, b bool) string {
	if b {
		return t.Success.Render("● on ")
	}
	return t.Muted.Render("○ off")
}

func (u *UI) runScript(script string, scope Scope) error {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.ps1", appName, time.Now().UnixNano()))
	if err := os.WriteFile(tmp, []byte(script), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)

	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", tmp)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if scope == ScopeMachine {
			fmt.Println("  " + u.t.Muted.Render("Machine scope requires Run as Administrator."))
		}
		return err
	}
	return nil
}

func (u *UI) save(rep *Report) {
	name := fmt.Sprintf("apply-path-%s-%s.ps1", rep.Scope, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(name, []byte(buildScript(rep)), 0o644); err != nil {
		fmt.Println("  " + u.t.Danger.Render("Write failed: "+err.Error()))
		return
	}
	abs, _ := filepath.Abs(name)
	fmt.Println("\n  " + u.t.Success.Render("✔ Script saved"))
	fmt.Println("    " + u.t.Path.Render(abs))
	fmt.Println("    " + u.t.Muted.Render(
		"Review it, then run: powershell -ExecutionPolicy Bypass -File \""+abs+"\""))
}

func (u *UI) exportJSON(rep *Report) {
	name := fmt.Sprintf("path-report-%s.json", time.Now().Format("20060102-150405"))
	data, _ := json.MarshalIndent(rep, "", "  ")
	if err := os.WriteFile(name, data, 0o644); err != nil {
		fmt.Println("  " + u.t.Danger.Render("Write failed: "+err.Error()))
		return
	}
	abs, _ := filepath.Abs(name)
	fmt.Println("\n  " + u.t.Success.Render("✔ Report exported"))
	fmt.Println("    " + u.t.Path.Render(abs))
}

func (u *UI) apply(rep *Report, opts Options) bool {
	if u.demo {
		fmt.Println("\n  " + u.t.Danger.Render(
			"Demo mode — refusing to write synthetic paths to your registry."))
		return false
	}
	if !rep.Scope.writable() {
		fmt.Println("\n  " + u.t.Danger.Render(
			"Refusing to write process-scope PATH — it is a merged view, not a real hive."))
		return false
	}
	if runtime.GOOS != "windows" {
		fmt.Println("\n  " + u.t.Warn.Render("Not on Windows. Use 's' to save the script instead."))
		return false
	}
	fmt.Println("\n" + u.t.Box.Render(
		u.t.Danger.Render("This rewrites the "+string(rep.Scope)+" PATH in the registry.")+"\n"+
			u.t.Muted.Render("A snapshot and a .reg backup are taken first.\n"+
				"Machine scope needs an elevated shell.")))
	if u.ask("  Type "+u.t.Chip.Render("APPLY")+" to confirm: ") != "APPLY" {
		fmt.Println("  " + u.t.Muted.Render("Cancelled."))
		return false
	}

	sn, err := capture(u.store, rep.Scope, "pre-apply", "automatic backup before apply")
	if err != nil {
		fmt.Println("  " + u.t.Warn.Render("Backup failed: "+err.Error()))
		if u.ask("  Continue without a backup? (yes/no): ") != "yes" {
			return false
		}
	} else if sn != nil {
		fmt.Printf("  %s %s\n", u.t.Success.Render("✔ Snapshot"),
			u.t.Muted.Render(fmt.Sprintf("#%d saved — restore with: %s --restore %d",
				sn.ID, appName, sn.ID)))
	}

	if err := u.runScript(buildScript(rep), rep.Scope); err != nil {
		fmt.Println("  " + u.t.Danger.Render("Apply failed: "+err.Error()))
		if u.store != nil {
			_ = u.store.SaveRun(rep, opts, false)
		}
		return false
	}
	if u.store != nil {
		_, _ = capture(u.store, rep.Scope, "post-apply", "state after apply")
		_ = u.store.SaveRun(rep, opts, true)
	}
	fmt.Println("\n  " + u.t.Success.Render("✔ Applied. Open a new terminal to pick it up.\n"))
	return true
}

func (u *UI) menu(opts *Options, rep *Report, scope Scope) {
	toggles := map[string]*bool{
		"1": &opts.Normalize, "2": &opts.Dedup, "3": &opts.Prune,
		"4": &opts.Tokenize, "5": &opts.Group, "6": &opts.AllowGrowth,
	}
	labels := []struct{ k, l string }{
		{"1", "Normalize entries"},
		{"2", "Remove duplicates"},
		{"3", "Prune dead directories"},
		{"4", "Tokenize with %VARS%"},
		{"5", "Group toolchains"},
		{"6", "Tokenize even if longer"},
	}
	for {
		fmt.Println("\n" + u.t.Rule(u.w))
		fmt.Println("  " + u.t.Subtitle.Render("OPTIMIZATIONS") +
			u.t.Muted.Render("   press a number to toggle"))
		for _, x := range labels {
			fmt.Printf("   %s %s  %s\n", u.t.Key.Render(x.k), onOff(u.t, *toggles[x.k]), x.l)
		}
		fmt.Println("\n  " + u.t.Subtitle.Render("ACTIONS"))
		fmt.Printf("   %s Full diff    %s Save .ps1    %s Export JSON\n",
			u.t.Key.Render("d"), u.t.Key.Render("s"), u.t.Key.Render("j"))
		fmt.Printf("   %s History      %s Restore      %s Apply now    %s Quit\n",
			u.t.Key.Render("h"), u.t.Key.Render("r"),
			u.t.Key.Render("a"), u.t.Key.Render("q"))
		fmt.Println(u.t.Rule(u.w))

		c := strings.ToLower(u.ask("  ❯ "))
		if p, ok := toggles[c]; ok {
			*p = !*p
			*rep = *Optimize(rep.Original, *opts, scope)
			u.summary(rep)
			continue
		}
		switch c {
		case "d":
			u.diff(rep)
		case "s":
			u.save(rep)
		case "j":
			u.exportJSON(rep)
		case "h":
			u.showHistory("", 20)
		case "r":
			if u.restore(0, scope) {
				return
			}
		case "a":
			if u.apply(rep, *opts) {
				return
			}
		case "q", "":
			fmt.Println("\n  " + u.t.Muted.Render("Nothing was changed. Bye.\n"))
			return
		default:
			fmt.Println("  " + u.t.Warn.Render("Unknown option."))
		}
	}
}
