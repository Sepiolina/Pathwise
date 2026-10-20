# Pathwise user guide

This guide walks through a full Pathwise session, explains how to read the report, and covers
applying, restoring and troubleshooting. Screenshots are from `pathwise --demo`, which uses
synthetic data, so you can follow along without touching your real environment.

- [1. Choose a scope](#1-choose-a-scope)
- [2. Read the report](#2-read-the-report)
- [3. Experiment with the optimizations](#3-experiment-with-the-optimizations)
- [4. Review the diff](#4-review-the-diff)
- [5. Apply, or export instead](#5-apply-or-export-instead)
- [6. Undo with history and restore](#6-undo-with-history-and-restore)
- [7. Scripting and automation](#7-scripting-and-automation)
- [Troubleshooting](#troubleshooting)

---

## 1. Choose a scope

Start Pathwise without `--scope` and it asks which `PATH` you want to inspect:

| Scope | Registry location | Notes |
|---|---|---|
| `user` | `HKCU\Environment` | Safe, no admin rights needed. The best place to start. |
| `machine` | `HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment` | System-wide. Applying needs an **elevated** shell. |
| `process` | the current process | Merged Machine + User view. **Read-only**, for diagnostics. |

```sh
pathwise --scope user        # recommended first run
pathwise --demo              # no real data involved
```

> **Machine scope and user variables.** A machine `PATH` cannot expand `%USERPROFILE%`,
> `%APPDATA%` or `%LOCALAPPDATA%`. Pathwise will not tokenize with those in machine scope and
> warns you if such entries are present. Move them to user scope.

## 2. Read the report

<img src="images/01-analysis.png" alt="Analysis report" width="820">

**Gauges.** `BEFORE` and `AFTER` show the `PATH` length in bytes against your budget
(`--budget`, default 2047). The bar and status change with the ratio:

| Status | Meaning |
|---|---|
| `HEALTHY` | Under 85 % of the budget |
| `APPROACHING LIMIT` | Between 85 % and 100 % of the budget |
| `OVER BUDGET` | Above the budget you set |
| `EXCEEDS REGISTRY LIMIT` | Above 32767 bytes, the registry's hard limit |

**Net result.** How many bytes were reclaimed, as a number and a percentage. If grouping was
used, the note underneath reminds you that grouped entries still exist. They only moved into
their own variables.

**Stage table.**

| Column | Meaning |
|---|---|
| `STAGE` | Which optimization ran (grouping shows one row per variable, e.g. `Group PATHS_MSSQL`) |
| `WHAT IT DID` | A one-line description |
| `ITEMS` | How many entries the stage touched |
| `PATH DELTA` | Bytes saved (green, `-`) or added (amber, `+`) |

Stages that did nothing are not listed. If nothing changed at all you will see
*"No changes. Your PATH is already clean."*

**Findings.** Lists of removed duplicates and dead directories (first six, then "… and N more"),
and the group variables that were created with their size and entry count.

**Warnings.** Amber lines call out things you should know, for example that a `PATH` over 1024
bytes must never be applied with `setx.exe`, or that demo mode cannot apply.

## 3. Experiment with the optimizations

<img src="images/02-menu.png" alt="Interactive menu" width="640">

Each number toggles one stage, and the pipeline re-runs immediately:

| Key | Stage | Turn it off when… |
|---|---|---|
| `1` | Normalize entries | You want entries left exactly as typed |
| `2` | Remove duplicates | You deliberately repeat an entry |
| `3` | Prune dead directories | A drive is temporarily disconnected, or you manage paths for another machine |
| `4` | Tokenize with `%VARS%` | You prefer literal paths |
| `5` | Group toolchains | You do not want `PATHS_*` variables created |
| `6` | Tokenize even if longer | *Off by default.* Turn **on** if you want tokens even when they do not shorten the entry |

<img src="images/03-toggle.png" alt="Report after turning pruning off" width="820">

Above, pruning was turned off. The dead directories stay, the saving drops, and the `AFTER`
gauge moves. You can compare freely: nothing is written until you apply.

> **Tip.** `--no-prune` starts with pruning disabled, and `--budget` changes the target size.

## 4. Review the diff

<img src="images/04-diff.png" alt="Full diff" width="700">

Press `d` for an entry-by-entry view in original order:

| Marker | Meaning |
|---|---|
| `−` red | Removed, with the reason in brackets (`duplicate` or `not found`) |
| `→` blue | Moved into a group variable, shown as a `%PATHS_*%` chip |
| `~` green | Rewritten (trimmed, or tokenized). The original appears underneath as `was: …` |
| `=` grey | Unchanged |

## 5. Apply, or export instead

Choose what to do with the result:

| Key | Action |
|---|---|
| `s` | **Save `.ps1`**: writes `apply-path-<scope>-<timestamp>.ps1` to the current folder. Review it, then run it yourself. |
| `j` | **Export JSON**: writes `path-report-<timestamp>.json` with every entry and stage. |
| `a` | **Apply now**: rewrites the registry (see below). |
| `q` | Quit. *"Nothing was changed."* |

<img src="images/05-apply.png" alt="Apply refused in demo mode" width="600">

Applying is refused in demo mode, for process scope, and on non-Windows systems. On a real
scope the flow is:

1. Pathwise explains what is about to happen.
2. You type `APPLY` to confirm.
3. A **pre-apply snapshot** and a `.reg` backup are taken. You are shown the snapshot ID and the
   exact restore command, e.g. `pathwise --restore 7`.
4. The generated PowerShell script runs. It writes group variables first, then `PATH`, using
   `Set-ItemProperty -Type ExpandString`, and finally broadcasts the change.
5. A **post-apply snapshot** is recorded.

Open a **new terminal** afterwards. Already-open terminals keep their old environment.

For machine scope, start your terminal with **Run as administrator**.

### What the generated script looks like

```powershell
#requires -Version 5.1
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# --- 1. Backup (restore by double-clicking the .reg file) ---
$backup = Join-Path $env:USERPROFILE 'env-backup-user-20261015-153934.reg'
reg export "HKCU\Environment" $backup /y | Out-Null

# --- 2. Group variables ---
Set-ItemProperty -Path 'HKCU:\Environment' -Name 'PATHS_MSSQL' -Value '%ProgramFiles%\Microsoft SQL Server\130\Tools\Binn;...' -Type ExpandString

# --- 3. PATH (ExpandString keeps %VARS% working; setx would truncate) ---
Set-ItemProperty -Path 'HKCU:\Environment' -Name 'Path' -Value 'C:\Windows\system32;...;%PATHS_MSSQL%;%PATHS_PYTHON%' -Type ExpandString

# --- Broadcast the change so new shells pick it up ---
# (SendMessageTimeout WM_SETTINGCHANGE "Environment")
```

The script is self-contained: it makes its own `.reg` backup even if you never use Pathwise's
history.

## 6. Undo with history and restore

Pathwise records two kinds of history in a local SQLite database
(`%LOCALAPPDATA%\pathwise\history.db`, override with `--db`):

- **Snapshots**: the raw `PATH` and every `PATHS_*` variable at a point in time.
- **Runs**: each analysis, with before/after sizes and whether it was applied.

Snapshot kinds:

| Kind | When it is taken |
|---|---|
| `auto` | On startup, if the registry `PATH` differs from the last recorded one |
| `pre-apply` / `post-apply` | Around **Apply now** |
| `pre-restore` / `post-restore` | Around a restore |

Everything except `auto` also gets a `.reg` file in `%LOCALAPPDATA%\pathwise\backups`.

```sh
pathwise --history        # list snapshots and runs
pathwise --restore 7      # restore snapshot #7 (type RESTORE to confirm)
pathwise --gc 20          # keep only the newest 20 auto snapshots per scope
```

You can also press `h` (history) or `r` (restore) in the menu. Restoring puts `PATH` and the
managed variables back exactly as they were, and **removes** any `PATHS_*` variable created after
that snapshot. It takes its own pre-restore snapshot first, so a restore can itself be undone.

Process-scope snapshots cannot be restored.

**No Pathwise on the machine?** Double-click the `.reg` file from the `backups` folder, or the
`env-backup-*.reg` file the script wrote to your profile folder.

## 7. Scripting and automation

```sh
# Report only, no prompts
pathwise --scope user --yes

# JSON for tooling
pathwise --scope user --json > report.json

# Generate the script, review it, run it later
pathwise --scope user --script cleanup.ps1

# Plain output for logs
pathwise --scope user --yes --no-color --width 100
```

Abridged JSON report:

```json
{
  "scope": "user",
  "generated_at": "2026-10-15T15:39:34Z",
  "optimized_path": "C:\\Windows\\system32;...;%PATHS_MSSQL%;%PATHS_PYTHON%",
  "original_length": 3654,
  "optimized_length": 1581,
  "budget": 2047,
  "stages": [
    { "name": "Normalize", "detail": "trimmed quotes, slashes & trailing separators",
      "before": 3654, "after": 3643, "touched": 10 }
  ],
  "group_variables": { "PATHS_MSSQL": "...", "PATHS_PYTHON": "..." },
  "entries": [ { "original": "...", "value": "...", "dropped": false, "exists": true } ]
}
```

Entries also carry `group` and `reason` (`duplicate` or `not found`) when they apply.

`--gc`, `--history` and `--restore` run and exit; `--json` and `--script` never write to the
registry.

## Troubleshooting

**"History disabled: …" appears at startup.** Pathwise could not open its database (read-only
profile, bad `--db` path). Analysis still works; snapshots do not. Use `--db` to pick a
writable location, or `--no-history` to silence it.

**My terminal still shows the old `PATH`.** Environment changes are picked up by *new*
processes. Close and reopen the terminal (and your IDE, which has its own copy).

**Machine scope: "Run as Administrator".** Applying to `HKLM` requires an elevated shell.
Analysis does not.

**An entry I wanted was removed as "not found".** Pathwise only prunes entries it confirmed are
missing. If a drive was offline, turn pruning off (`3`) or run with `--no-prune`, and restore
from the pre-apply snapshot if you already applied.

**Entries with `%PATHS_…%` are never pruned.** Pathwise cannot resolve variables this process
does not have yet (for example group variables it just wrote, before you reopened the
terminal), so it leaves those entries alone rather than guess. For the same reason, a literal
path and an unresolvable-variable entry pointing at the same folder are not recognised as
duplicates. That is a missed cleanup, never a destructive one.

**"Process scope is READ-ONLY".** Process scope is a merged view. Re-run with `--scope user` or
`--scope machine` to make changes.

**Running on macOS or Linux.** See [Running outside Windows](../README.md#running-outside-windows):
analysis and `--script` work; applying does not.
