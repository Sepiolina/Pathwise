package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// ---------------------------------------------------------------- main

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func main() {
	var (
		scopeFlag = flag.String("scope", "", "user | machine | process (prompted if omitted)")
		budget    = flag.Int("budget", defaultBudget, "target maximum PATH length in bytes")
		demo      = flag.Bool("demo", false, "run against a synthetic bloated PATH (cannot apply)")
		noColor   = flag.Bool("no-color", false, "disable ANSI colors")
		jsonOut   = flag.Bool("json", false, "print a JSON report and exit")
		scriptOut = flag.String("script", "", "write the PowerShell script to this file and exit")
		yes       = flag.Bool("yes", false, "non-interactive: analyze, report, exit")
		noPrune   = flag.Bool("no-prune", false, "keep directories that no longer exist")
		width     = flag.Int("width", 76, "output width")
		dbPath    = flag.String("db", "", "history database path (default: %LOCALAPPDATA%\\pathwise)")
		noHistory = flag.Bool("no-history", false, "disable snapshots and run logging")
		history   = flag.Bool("history", false, "show snapshot & run history, then exit")
		restoreID = flag.Int64("restore", 0, "restore snapshot by ID (use --history to list)")
		gcKeep    = flag.Int("gc", 0, "delete auto snapshots beyond the newest N per scope")
	)
	flag.Parse()

	if *budget <= 0 {
		fmt.Fprintln(os.Stderr, "pathwise: --budget must be a positive number of bytes")
		os.Exit(2)
	}
	if *budget > hardLimit {
		fmt.Fprintf(os.Stderr, "pathwise: --budget %d exceeds the registry's hard limit of %d bytes\n",
			*budget, hardLimit)
		os.Exit(2)
	}
	if *width < 20 {
		fmt.Fprintln(os.Stderr, "pathwise: --width must be at least 20")
		os.Exit(2)
	}
	if *gcKeep < 0 {
		fmt.Fprintln(os.Stderr, "pathwise: --gc must not be negative")
		os.Exit(2)
	}

	useColor := !*noColor && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	theme := NewTheme(useColor)
	ui := &UI{t: theme, w: *width, stdin: bufio.NewReader(os.Stdin), demo: *demo}

	if !*noHistory {
		st, err := OpenStore(*dbPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, theme.Warn.Render(
				"  ⚠ History disabled: "+err.Error()))
		} else {
			ui.store = st
			defer st.Close()
		}
	}

	interactive := !*yes && !*jsonOut && *scriptOut == "" && isTTY()

	// Resolve scope.
	scope := Scope(strings.ToLower(*scopeFlag))
	if scope != ScopeUser && scope != ScopeMachine && scope != ScopeProcess {
		if !interactive || *history || *restoreID > 0 {
			scope = ScopeUser
		} else {
			ui.header("—", "not selected")
			fmt.Println("  " + theme.Subtitle.Render("Which PATH do you want to inspect?"))
			fmt.Printf("   %s User      %s\n", theme.Key.Render("1"),
				theme.Muted.Render("HKCU\\Environment — safe, no admin needed"))
			fmt.Printf("   %s Machine   %s\n", theme.Key.Render("2"),
				theme.Muted.Render("system-wide — requires an elevated shell to apply"))
			fmt.Printf("   %s Process   %s\n\n", theme.Key.Render("3"),
				theme.Muted.Render("merged view — read-only diagnostics"))
			switch ui.ask("  ❯ ") {
			case "2":
				scope = ScopeMachine
			case "3":
				scope = ScopeProcess
			default:
				scope = ScopeUser
			}
		}
	}

	// Maintenance / history-only modes.
	if *gcKeep > 0 && ui.store != nil {
		n, err := ui.store.GC(*gcKeep)
		if err != nil {
			fmt.Fprintln(os.Stderr, theme.Danger.Render(err.Error()))
			os.Exit(1)
		}
		fmt.Printf("%s %d auto snapshots pruned\n", theme.Success.Render("✔"), n)
		return
	}
	if *history {
		ui.showHistory("", 25)
		fmt.Println()
		return
	}
	if *restoreID > 0 {
		if !ui.restore(*restoreID, scope) {
			os.Exit(1)
		}
		return
	}

	// Record the current state before we do anything else.
	if !*demo {
		captureIfChanged(ui.store, scope)
	}

	raw, source := "", ""
	if *demo {
		raw, source = demoPath(), "synthetic demo data"
	} else {
		var err error
		raw, source, err = readPath(scope)
		if err != nil {
			fmt.Fprintln(os.Stderr, theme.Danger.Render("  ✗ "+err.Error()))
			fmt.Fprintln(os.Stderr, theme.Muted.Render("    Try --demo to preview the tool."))
			os.Exit(1)
		}
	}

	opts := Options{
		Normalize: true, Dedup: true, Prune: !*noPrune,
		Tokenize: true, Group: true, Budget: *budget,
	}
	rep := Optimize(raw, opts, scope)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}
	if *scriptOut != "" {
		if err := os.WriteFile(*scriptOut, []byte(buildScript(rep)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(theme.Success.Render("✔ ") + *scriptOut)
		return
	}

	ui.header(scope, source)
	ui.summary(rep)

	if !interactive {
		if ui.store != nil && !*demo {
			_ = ui.store.SaveRun(rep, opts, false)
		}
		fmt.Println("\n  " + theme.Muted.Render(
			"Run without --yes for the interactive menu, or use --script to emit a .ps1.\n"))
		return
	}
	ui.menu(&opts, rep, scope)
}
