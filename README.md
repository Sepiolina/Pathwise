# Pathwise

A Windows `PATH` environment variable analyzer, optimizer, and time machine.

`PATH` variables have a way of turning into landfills — duplicate entries, directories
for uninstalled software, half a dozen ways of spelling the same SDK folder. Eventually
Windows starts silently truncating it, or `cmd.exe` and older tools choke on it. Pathwise
finds out exactly what's in there, shows you what's safe to remove or rewrite, and lets
you apply the change with a backup and an undo button.

Nothing is written to the registry until you explicitly confirm it.

## Features

- **Reads the real value.** Pulls `PATH` directly from the registry
  (`HKCU\Environment` or `HKLM\...\Environment`) without expanding `%VARIABLES%`, so
  round-trips don't silently resolve tokens into literal paths.
- **A five-stage optimization pipeline**, each stage independently toggleable:

  | Stage | What it does |
  |---|---|
  | Normalize | Trims quotes/whitespace, collapses `/` and `\\`, strips trailing separators |
  | Deduplicate | Removes repeated directories (case-insensitive, first occurrence wins) |
  | Prune dead paths | Removes directories that no longer exist on disk (skips on permission errors — never guesses) |
  | Tokenize | Swaps literal prefixes like `C:\Program Files` for `%ProgramFiles%`, only when it actually shortens the entry |
  | Group toolchains | Hoists noisy SDK/toolchain directories (SQL Server, .NET, Java, Node, Python) into their own `PATHS_*` variables when two or more entries match |

- **A byte budget, not just a length.** Warns as you approach a practical ceiling
  (2047 bytes, where legacy tooling and `cmd.exe` start breaking) and errors out before
  you'd hit the registry's hard limit (32767 bytes). Always warns you away from
  `setx.exe`, which silently truncates anything over 1024 bytes.
- **A full history.** Every run and every registry write is recorded locally in SQLite
  (`%LOCALAPPDATA%\pathwise\history.db` by default), alongside independent `.reg` file
  backups. Snapshots can be listed and restored later.
- **Three scopes**: `user` (`HKCU`, no admin needed), `machine` (`HKLM`, needs an
  elevated shell to apply), and `process` (the merged view your current shell sees —
  read-only, since writing it back would corrupt one of the other two).
- **Demo mode** (`--demo`) runs the whole pipeline against a synthetic, deliberately
  bloated `PATH` so you can try the tool without touching your real environment.
  Applying is disabled in this mode.

## Installation

Requires Go 1.26+.

```sh
git clone https://github.com/<your-org>/pathwise.git
cd pathwise
go build -o pathwise.exe .
```

The registry read/write, snapshot/restore, and script-execution features are
Windows-only. On other platforms, Pathwise still builds and runs, but falls back to
analyzing the current process's `PATH` and can only emit a script or JSON report — see
[Running outside Windows](#running-outside-windows).

## Quick start

```sh
# Try it without touching anything real
pathwise --demo

# Analyze your real user PATH, then walk through the interactive menu
pathwise --scope user

# Just get a report, no prompts
pathwise --scope user --yes

# Get the change as a script instead of applying it yourself
pathwise --scope user --script cleanup.ps1
```

If you omit `--scope`, Pathwise asks which one you mean before doing anything.

## The interactive menu

```
  OPTIMIZATIONS   press a number to toggle
   1 ● on   Normalize entries
   2 ● on   Remove duplicates
   3 ● on   Prune dead directories
   4 ● on   Tokenize with %VARS%
   5 ● on   Group toolchains
   6 ○ off  Tokenize even if longer

  ACTIONS
   d Full diff    s Save .ps1    j Export JSON
   h History      r Restore      a Apply now    q Quit
```

Toggling an optimization re-runs the whole pipeline live so you can see the effect
before committing to anything.

- **d — Full diff**: entry-by-entry before/after, including why each dropped entry was
  dropped.
- **s — Save .ps1**: writes the generated PowerShell script to disk without running it.
- **j — Export JSON**: writes the full `Report` (including every entry and stage) as
  JSON.
- **h — History**: lists recorded snapshots and past runs for this machine.
- **r — Restore**: rolls a scope's `PATH` (and any Pathwise-managed variables) back to
  a previous snapshot.
- **a — Apply now**: takes a fresh pre-apply snapshot, then runs the generated
  PowerShell script to actually rewrite the registry. Requires typing `APPLY` to
  confirm.

## CLI flags

| Flag | Default | Description |
|---|---|---|
| `--scope` | *(prompted)* | `user`, `machine`, or `process` |
| `--budget` | `2047` | Target maximum `PATH` length in bytes |
| `--demo` | `false` | Run against synthetic bloated `PATH` data (cannot apply) |
| `--yes` | `false` | Non-interactive: analyze, print the report, exit |
| `--json` | `false` | Print a JSON report and exit |
| `--script <file>` | | Write the PowerShell script to a file and exit, without running it |
| `--no-prune` | `false` | Keep directories that no longer exist on disk |
| `--no-color` | `false` | Disable ANSI colors (also respects `NO_COLOR` / `TERM=dumb`) |
| `--width` | `76` | Output width for tables and bars |
| `--db <path>` | `%LOCALAPPDATA%\pathwise\history.db` | History database location |
| `--no-history` | `false` | Disable snapshots and run logging entirely |
| `--history` | `false` | Show snapshot & run history, then exit |
| `--restore <id>` | | Restore a snapshot by ID (use `--history` to find one), then exit |
| `--gc <n>` | | Delete automatic snapshots beyond the newest `n` per scope, then exit |

## Safety model

Pathwise treats writing to the registry as the one operation worth being paranoid
about. Specifically:

1. **Analysis is always safe.** Running Pathwise with no flags, `--yes`, `--json`, or
   `--script` never touches the registry — it only reads.
2. **Applying requires an explicit, typed confirmation** (`APPLY` or `RESTORE`), shown
   next to a plain description of what's about to happen.
3. **A snapshot is taken immediately before every write** — both when applying an
   optimization and before restoring an older snapshot — independent of whether the
   write itself succeeds.
4. **A `.reg` file is exported alongside every non-automatic snapshot**, in
   `%LOCALAPPDATA%\pathwise\backups`, so you can recover by double-clicking a file even
   without Pathwise installed.
5. **Writes go through a generated PowerShell script**, using
   `Set-ItemProperty -Type ExpandString`, never `setx.exe` (which silently truncates
   past 1024 bytes) and never a naive string overwrite that would lose existing
   `%VARIABLES%`.
6. **New shells are notified.** The script broadcasts `WM_SETTINGCHANGE` after writing,
   so newly opened terminals pick up the change without a reboot (existing terminals
   still need to be reopened).
7. **Process scope is read-only.** It's a merged view of Machine + User `PATH`, and
   writing it back to either hive would corrupt your environment — Pathwise refuses.

## Running outside Windows

The registry, snapshot/restore, and script-execution features depend on the Windows
registry and PowerShell, so on other platforms Pathwise:

- always analyzes the current process's `PATH`, regardless of `--scope`;
- can still run the full optimization pipeline and print a report (`--yes`, `--json`);
- can still emit the equivalent PowerShell script with `--script`, for use on a Windows
  machine;
- refuses to apply or restore, with a message pointing you at `--script` instead.

This makes it possible to develop and test Pathwise's optimization logic on macOS/Linux,
even though its intended target is Windows.

## Building from source

```sh
go build -o pathwise.exe .
go vet ./...
go test ./...
```

Pathwise has no CGo dependency — `modernc.org/sqlite` is a pure-Go SQLite driver, so
cross-compiling (e.g. from Linux to `windows/amd64`) works with a plain
`GOOS=windows GOARCH=amd64 go build`.

## Source layout

The project is a single `package main`, split across files by responsibility rather
than one large file:

| File | Contents |
|---|---|
| `types.go` | Shared constants, `Scope`, and the `Entry` / `Stage` / `Options` / `Report` model |
| `theme.go` | Terminal styling (`Theme`, built on `lipgloss`) |
| `tokens.go` | Special-folder token detection and toolchain grouping rules |
| `pathutil.go` | Pure string helpers: `hasPathPrefix`, `expandWin`, `normalizePath`, `dedupKey`, `buildPath` |
| `engine.go` | `Optimize()` — the five-stage pipeline described above |
| `registry.go` | Reading `PATH` and managed variables from the Windows registry via PowerShell |
| `history.go` | The SQLite-backed snapshot/run history (`Store`) |
| `script.go` | Generating the PowerShell apply/restore scripts |
| `ui.go` | Terminal rendering: tables, the summary/diff views, and the interactive menu |
| `main.go` | Flag parsing and wiring everything together |

`pathutil.go` and `engine.go` contain no registry, filesystem-mutating, or terminal
I/O — they're covered by `pathutil_test.go` and `engine_test.go`, which run on any OS
and don't touch the real environment:

```sh
go test ./... -v
```

`engine_test.go` exercises each optimization stage (normalize, dedup, prune, tokenize,
group) individually and in combination, using `t.TempDir()` for real-vs-dead directory
checks and `t.Setenv()` for token detection, so nothing outside the test's own process
is touched.

## License

MIT — see [LICENSE](LICENSE).
