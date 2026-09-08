package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"flag"
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
	_ "modernc.org/sqlite"
)

const (
	appName    = "pathwise"
	appVersion = "1.0.0"
	sep        = ";"

	// Practical ceiling for the expanded PATH before legacy tooling and
	// cmd.exe start truncating. The registry itself tolerates 32767.
	defaultBudget = 2047
	hardLimit     = 32767
	setxLimit     = 1024 // setx.exe silently truncates past this. Never use it.

	// Variables the tool creates and is therefore allowed to delete on restore.
	managedPrefix = "PATHS_"
)

// ---------------------------------------------------------------- scope

type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeMachine Scope = "machine"
	ScopeProcess Scope = "process"
)

func (s Scope) hive() string {
	if s == ScopeMachine {
		return `HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	}
	return `HKCU\Environment`
}

func (s Scope) psPath() string {
	if s == ScopeMachine {
		return `HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
	}
	return `HKCU:\Environment`
}

func (s Scope) writable() bool { return s == ScopeUser || s == ScopeMachine }

// ---------------------------------------------------------------- theme

type Theme struct {
	Title, Subtitle             lipgloss.Style
	Success, Warn, Danger, Info lipgloss.Style
	Muted, Chip, Box, Key, Path lipgloss.Style
	Border                      lipgloss.Style
}

func NewTheme(color bool) *Theme {
	if !color {
		p := lipgloss.NewStyle()
		return &Theme{
			Title: p.Bold(true), Subtitle: p.Bold(true),
			Success: p, Warn: p, Danger: p, Info: p, Muted: p,
			Chip: p, Key: p, Path: p, Border: p,
			Box: p.Border(lipgloss.NormalBorder()).Padding(1, 2),
		}
	}
	return &Theme{
		Title: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#7D56F4")).
			Padding(0, 2),
		Subtitle: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#EE6FF8")),
		Success:  lipgloss.NewStyle().Foreground(lipgloss.Color("#04B575")),
		Warn:     lipgloss.NewStyle().Foreground(lipgloss.Color("#F5A623")),
		Danger:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF5F87")),
		Info:     lipgloss.NewStyle().Foreground(lipgloss.Color("#00BFFF")),
		Muted:    lipgloss.NewStyle().Foreground(lipgloss.Color("#737373")),
		Path:     lipgloss.NewStyle().Foreground(lipgloss.Color("#B4B4B4")),
		Key: lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("#1A1A1A")).
			Background(lipgloss.Color("#00BFFF")).Padding(0, 1),
		Chip: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FAFAFA")).
			Background(lipgloss.Color("#434343")).Padding(0, 1),
		Border: lipgloss.NewStyle().Foreground(lipgloss.Color("#5A5A5A")),
		Box: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#5A5A5A")).
			Padding(1, 2),
	}
}

func (t *Theme) Bar(cur, budget, width int) string {
	if budget <= 0 {
		budget = 1
	}
	ratio := float64(cur) / float64(budget)
	filled := int(ratio * float64(width))
	if filled > width {
		filled = width
	}
	if filled < 0 {
		filled = 0
	}
	st := t.Success
	switch {
	case ratio >= 1.0:
		st = t.Danger
	case ratio >= 0.85:
		st = t.Warn
	}
	return st.Render(strings.Repeat("█", filled)) +
		t.Muted.Render(strings.Repeat("░", width-filled))
}

func (t *Theme) Rule(w int) string { return t.Border.Render(strings.Repeat("─", w)) }

// ---------------------------------------------------------------- model

type Entry struct {
	Original string `json:"original"`
	Value    string `json:"value"`
	Group    string `json:"group,omitempty"`
	Dropped  bool   `json:"dropped"`
	Reason   string `json:"reason,omitempty"`
	Exists   bool   `json:"exists"`
	Checked  bool   `json:"-"`
}

type Stage struct {
	Name    string `json:"name"`
	Detail  string `json:"detail"`
	Before  int    `json:"before"`
	After   int    `json:"after"`
	Touched int    `json:"touched"`
}

func (s Stage) Delta() int { return s.Before - s.After }

type Options struct {
	Normalize   bool `json:"normalize"`
	Dedup       bool `json:"dedup"`
	Prune       bool `json:"prune"`
	Tokenize    bool `json:"tokenize"`
	Group       bool `json:"group"`
	AllowGrowth bool `json:"allow_growth"`
	Budget      int  `json:"budget"`
}

type Report struct {
	Scope        Scope             `json:"scope"`
	GeneratedAt  string            `json:"generated_at"`
	Original     string            `json:"-"`
	Optimized    string            `json:"optimized_path"`
	OriginalLen  int               `json:"original_length"`
	OptimizedLen int               `json:"optimized_length"`
	Budget       int               `json:"budget"`
	Stages       []Stage           `json:"stages"`
	Groups       map[string]string `json:"group_variables"`
	GroupOrder   []string          `json:"-"`
	Entries      []*Entry          `json:"entries"`
}

func (r *Report) Saved() int { return r.OriginalLen - r.OptimizedLen }

// ---------------------------------------------------------------- tokens

type Token struct {
	Name     string
	Value    string
	UserOnly bool // not expanded in Machine-scope PATH
}

func detectTokens() []Token {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	home := get("USERPROFILE", `C:\Users\Default`)
	toks := []Token{
		{"ProgramFiles(x86)", get("ProgramFiles(x86)", `C:\Program Files (x86)`), false},
		{"ProgramFiles", get("ProgramFiles", `C:\Program Files`), false},
		{"ProgramData", get("ProgramData", `C:\ProgramData`), false},
		{"SystemRoot", get("SystemRoot", `C:\Windows`), false},
		{"LOCALAPPDATA", get("LOCALAPPDATA", home+`\AppData\Local`), true},
		{"APPDATA", get("APPDATA", home+`\AppData\Roaming`), true},
		{"USERPROFILE", home, true},
	}
	sort.SliceStable(toks, func(i, j int) bool {
		return len(toks[i].Value) > len(toks[j].Value)
	})
	return toks
}

// ---------------------------------------------------------------- groups

type GroupRule struct {
	Var      string
	Keywords []string
}

var defaultGroups = []GroupRule{
	{managedPrefix + "MSSQL", []string{"sql server", "mssql", "azure data studio", "odbc"}},
	{managedPrefix + "DOTNET", []string{`\dotnet`, "visual studio", "msbuild", `\.net`}},
	{managedPrefix + "JAVA", []string{`\jdk`, `\jre`, `\java`, "maven", "gradle"}},
	{managedPrefix + "NODE", []string{"nodejs", `\npm`, `\yarn`, "pnpm"}},
	{managedPrefix + "PYTHON", []string{"python", "anaconda", "miniconda"}},
}

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

func expandWin(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '%' {
			if j := strings.IndexByte(s[i+1:], '%'); j > 0 {
				name := s[i+1 : i+1+j]
				if v, ok := os.LookupEnv(name); ok {
					b.WriteString(v)
					i += j + 2
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
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
				e.Checked = true
				fi, err := os.Stat(expandWin(e.Value))
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

// ---------------------------------------------------------------- registry

func runPS(script string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-Command", script).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

func regRoot(scope Scope) string {
	if scope == ScopeMachine {
		return `[Microsoft.Win32.Registry]::LocalMachine.OpenSubKey('SYSTEM\CurrentControlSet\Control\Session Manager\Environment')`
	}
	return `[Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')`
}

// readRegValue returns the RAW value so existing %VARS% survive round-trips.
// .NET's GetEnvironmentVariable expands them; we explicitly opt out.
func readRegValue(scope Scope, name string) (string, error) {
	ps := fmt.Sprintf(
		`$k=%s; if($k){$k.GetValue('%s','',[Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)}`,
		regRoot(scope), name)
	return runPS(ps)
}

// listManagedVars returns existing PATHS_* variable names in the given scope.
func listManagedVars(scope Scope) ([]string, error) {
	if runtime.GOOS != "windows" || !scope.writable() {
		return nil, nil
	}
	ps := fmt.Sprintf(
		`(Get-Item -Path '%s').Property | Where-Object { $_ -like '%s*' }`,
		scope.psPath(), managedPrefix)
	out, err := runPS(ps)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			names = append(names, l)
		}
	}
	sort.Strings(names)
	return names, nil
}

func readPath(scope Scope) (string, string, error) {
	if scope == ScopeProcess || runtime.GOOS != "windows" {
		return os.Getenv("PATH"), "process environment", nil
	}
	v, err := readRegValue(scope, "Path")
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", scope.hive(), err)
	}
	if strings.TrimSpace(v) == "" {
		return "", "", fmt.Errorf("no Path value found in %s", scope.hive())
	}
	return strings.TrimSpace(v), scope.hive(), nil
}

func demoPath() string {
	home := `C:\Users\Developer`
	root := `C:\Windows`
	p := []string{
		root + `\system32`, root, root + `\System32\Wbem`,
		root + `\System32\WindowsPowerShell\v1.0\`,
		root + `\system32`, // duplicate, different case
		`"C:\Program Files\Git\cmd"`,
		home + `\AppData\Local\Programs\Python\Python39\Scripts\`,
		home + `\AppData\Local\Programs\Python\Python39\`,
		home + `\AppData\Local\Microsoft\WindowsApps`,
		`C:\Program Files\Microsoft SQL Server\130\Tools\Binn\`,
		`C:\Program Files\Microsoft SQL Server\Client SDK\ODBC\170\Tools\Binn\`,
		`C:\Program Files (x86)\Microsoft SQL Server\150\DTS\Binn\`,
		`C:\Program Files\Microsoft SQL Server\150\Tools\Binn\`,
		`C:\Program Files\Azure Data Studio\bin`,
		`C:\Program Files\nodejs\`, `C:\Program Files\dotnet\`,
		`C:\DefinitelyDoesNotExist\bin`,
	}
	for i := 0; i < 22; i++ {
		p = append(p,
			fmt.Sprintf(`C:\Program Files\SomeRandomLongCompanyName\ApplicationFolder\Version%d\bin`, i),
			fmt.Sprintf(`%s\AppData\Roaming\SomeUserSpecificApp%d\bin`, home, i))
	}
	return strings.Join(p, sep)
}

// ---------------------------------------------------------------- history

type Snapshot struct {
	ID      int64
	TakenAt time.Time
	Scope   Scope
	Kind    string // auto | pre-apply | post-apply | pre-restore | manual
	Path    string
	Length  int
	RegFile string
	Note    string
	Vars    map[string]string
}

type Run struct {
	ID        int64
	RanAt     time.Time
	Scope     Scope
	BeforeLen int
	AfterLen  int
	Applied   bool
}

type Store struct {
	db   *sql.DB
	File string
}

func dataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		if h, err := os.UserHomeDir(); err == nil {
			base = filepath.Join(h, ".local", "share")
		} else {
			base = "."
		}
	}
	return filepath.Join(base, appName)
}

const schema = `
CREATE TABLE IF NOT EXISTS snapshots (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  taken_at TEXT    NOT NULL,
  scope    TEXT    NOT NULL,
  kind     TEXT    NOT NULL,
  path_raw TEXT    NOT NULL,
  length   INTEGER NOT NULL,
  reg_file TEXT    NOT NULL DEFAULT '',
  note     TEXT    NOT NULL DEFAULT '',
  host     TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS snapshot_vars (
  snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,
  value       TEXT    NOT NULL,
  PRIMARY KEY (snapshot_id, name)
);
CREATE TABLE IF NOT EXISTS runs (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  ran_at      TEXT    NOT NULL,
  scope       TEXT    NOT NULL,
  before_len  INTEGER NOT NULL,
  after_len   INTEGER NOT NULL,
  applied     INTEGER NOT NULL DEFAULT 0,
  options     TEXT    NOT NULL DEFAULT '{}',
  report_json TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_snap_scope ON snapshots(scope, taken_at DESC);
`

func OpenStore(path string) (*Store, error) {
	if path == "" {
		if err := os.MkdirAll(dataDir(), 0o755); err != nil {
			return nil, err
		}
		path = filepath.Join(dataDir(), "history.db")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite + single process: avoids lock contention
	if _, err = db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, File: path}, nil
}

func (s *Store) Close() {
	if s != nil && s.db != nil {
		s.db.Close()
	}
}

func (s *Store) Save(sn *Snapshot) (int64, error) {
	host, _ := os.Hostname()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(
		`INSERT INTO snapshots (taken_at, scope, kind, path_raw, length, reg_file, note, host)
		 VALUES (?,?,?,?,?,?,?,?)`,
		sn.TakenAt.Format(time.RFC3339), string(sn.Scope), sn.Kind,
		sn.Path, len(sn.Path), sn.RegFile, sn.Note, host)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for k, v := range sn.Vars {
		if _, err := tx.Exec(
			`INSERT INTO snapshot_vars (snapshot_id, name, value) VALUES (?,?,?)`,
			id, k, v); err != nil {
			return 0, err
		}
	}
	sn.ID = id
	return id, tx.Commit()
}

// LatestPath returns the most recent recorded PATH for a scope.
func (s *Store) LatestPath(scope Scope) (string, bool) {
	var p string
	err := s.db.QueryRow(
		`SELECT path_raw FROM snapshots WHERE scope=? ORDER BY id DESC LIMIT 1`,
		string(scope)).Scan(&p)
	if err != nil {
		return "", false
	}
	return p, true
}

func (s *Store) List(scope Scope, limit int) ([]*Snapshot, error) {
	q := `SELECT id, taken_at, scope, kind, path_raw, length, reg_file, note
	      FROM snapshots`
	args := []any{}
	if scope != "" {
		q += ` WHERE scope=?`
		args = append(args, string(scope))
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Snapshot
	for rows.Next() {
		sn := &Snapshot{Vars: map[string]string{}}
		var ts, sc string
		if err := rows.Scan(&sn.ID, &ts, &sc, &sn.Kind, &sn.Path,
			&sn.Length, &sn.RegFile, &sn.Note); err != nil {
			return nil, err
		}
		sn.TakenAt, _ = time.Parse(time.RFC3339, ts)
		sn.Scope = Scope(sc)
		out = append(out, sn)
	}
	return out, rows.Err()
}

func (s *Store) Get(id int64) (*Snapshot, error) {
	sn := &Snapshot{Vars: map[string]string{}}
	var ts, sc string
	err := s.db.QueryRow(
		`SELECT id, taken_at, scope, kind, path_raw, length, reg_file, note
		 FROM snapshots WHERE id=?`, id).
		Scan(&sn.ID, &ts, &sc, &sn.Kind, &sn.Path, &sn.Length, &sn.RegFile, &sn.Note)
	if err != nil {
		return nil, fmt.Errorf("snapshot %d not found", id)
	}
	sn.TakenAt, _ = time.Parse(time.RFC3339, ts)
	sn.Scope = Scope(sc)

	rows, err := s.db.Query(
		`SELECT name, value FROM snapshot_vars WHERE snapshot_id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		sn.Vars[k] = v
	}
	return sn, rows.Err()
}

func (s *Store) SaveRun(rep *Report, opts Options, applied bool) error {
	o, _ := json.Marshal(opts)
	r, _ := json.Marshal(rep)
	_, err := s.db.Exec(
		`INSERT INTO runs (ran_at, scope, before_len, after_len, applied, options, report_json)
		 VALUES (?,?,?,?,?,?,?)`,
		time.Now().Format(time.RFC3339), string(rep.Scope),
		rep.OriginalLen, rep.OptimizedLen, boolInt(applied), string(o), string(r))
	return err
}

func (s *Store) Runs(limit int) ([]Run, error) {
	rows, err := s.db.Query(
		`SELECT id, ran_at, scope, before_len, after_len, applied
		 FROM runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		var ts, sc string
		var ap int
		if err := rows.Scan(&r.ID, &ts, &sc, &r.BeforeLen, &r.AfterLen, &ap); err != nil {
			return nil, err
		}
		r.RanAt, _ = time.Parse(time.RFC3339, ts)
		r.Scope, r.Applied = Scope(sc), ap == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// GC keeps the newest N snapshots per scope, plus anything from an apply.
func (s *Store) GC(keep int) (int64, error) {
	res, err := s.db.Exec(`
		DELETE FROM snapshots
		WHERE kind = 'auto' AND id NOT IN (
		  SELECT id FROM snapshots a
		  WHERE a.scope = snapshots.scope
		  ORDER BY id DESC LIMIT ?
		)`, keep)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// capture reads live registry state and records it, exporting a .reg alongside.
func capture(st *Store, scope Scope, kind, note string) (*Snapshot, error) {
	if st == nil || !scope.writable() || runtime.GOOS != "windows" {
		return nil, nil
	}
	raw, err := readRegValue(scope, "Path")
	if err != nil {
		return nil, err
	}
	sn := &Snapshot{
		TakenAt: time.Now(), Scope: scope, Kind: kind,
		Path: strings.TrimSpace(raw), Note: note, Vars: map[string]string{},
	}
	if names, err := listManagedVars(scope); err == nil {
		for _, n := range names {
			if v, err := readRegValue(scope, n); err == nil {
				sn.Vars[n] = strings.TrimSpace(v)
			}
		}
	}
	// Belt and braces: a .reg file readable without this tool.
	if kind != "auto" {
		dir := filepath.Join(dataDir(), "backups")
		if os.MkdirAll(dir, 0o755) == nil {
			f := filepath.Join(dir, fmt.Sprintf("%s-%s-%s.reg",
				scope, kind, sn.TakenAt.Format("20060102-150405")))
			if err := exec.Command("reg", "export", scope.hive(), f, "/y").Run(); err == nil {
				sn.RegFile = f
			}
		}
	}
	if _, err := st.Save(sn); err != nil {
		return nil, err
	}
	return sn, nil
}

// captureIfChanged avoids filling the DB with identical auto snapshots.
func captureIfChanged(st *Store, scope Scope) {
	if st == nil || !scope.writable() || runtime.GOOS != "windows" {
		return
	}
	cur, err := readRegValue(scope, "Path")
	if err != nil {
		return
	}
	if last, ok := st.LatestPath(scope); ok && last == strings.TrimSpace(cur) {
		return
	}
	_, _ = capture(st, scope, "auto", "detected on startup")
}

// ---------------------------------------------------------------- script

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

const psBroadcast = `# --- Broadcast the change so new shells pick it up ---
if (-not ('Win32.NativeMethods' -as [type])) {
  Add-Type -Namespace Win32 -Name NativeMethods -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam,
    string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
}
$r = [UIntPtr]::Zero
[Win32.NativeMethods]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero,
    'Environment', 2, 5000, [ref]$r) | Out-Null
`

func psPreamble(scope Scope) string {
	var b strings.Builder
	b.WriteString("#requires -Version 5.1\n")
	if scope == ScopeMachine {
		b.WriteString("#requires -RunAsAdministrator\n")
	}
	b.WriteString("Set-StrictMode -Version Latest\n$ErrorActionPreference = 'Stop'\n\n")
	return b.String()
}

func buildScript(rep *Report) string {
	if !rep.Scope.writable() {
		return "# Process scope is read-only. Re-run with --scope user or --scope machine.\n"
	}
	ts := time.Now().Format("20060102-150405")
	var b strings.Builder

	fmt.Fprintf(&b, "# Generated by %s v%s on %s\n", appName, appVersion, rep.GeneratedAt)
	fmt.Fprintf(&b, "# Scope: %s   %d B -> %d B (saved %d B)\n",
		rep.Scope, rep.OriginalLen, rep.OptimizedLen, rep.Saved())
	b.WriteString(psPreamble(rep.Scope))

	b.WriteString("# --- 1. Backup (restore by double-clicking the .reg file) ---\n")
	fmt.Fprintf(&b, "$backup = Join-Path $env:USERPROFILE 'env-backup-%s-%s.reg'\n", rep.Scope, ts)
	fmt.Fprintf(&b, "reg export \"%s\" $backup /y | Out-Null\n", rep.Scope.hive())
	b.WriteString("Write-Host \"Backup written to $backup\" -ForegroundColor Green\n\n")

	if len(rep.GroupOrder) > 0 {
		b.WriteString("# --- 2. Group variables ---\n")
		for _, g := range rep.GroupOrder {
			fmt.Fprintf(&b,
				"Set-ItemProperty -Path '%s' -Name '%s' -Value %s -Type ExpandString\n",
				rep.Scope.psPath(), g, psQuote(rep.Groups[g]))
		}
		b.WriteString("\n")
	}

	b.WriteString("# --- 3. PATH (ExpandString keeps %VARS% working; setx would truncate) ---\n")
	fmt.Fprintf(&b, "Set-ItemProperty -Path '%s' -Name 'Path' -Value %s -Type ExpandString\n\n",
		rep.Scope.psPath(), psQuote(rep.Optimized))

	b.WriteString(psBroadcast)
	b.WriteString("Write-Host 'PATH updated. Open a new terminal to use it.' -ForegroundColor Green\n")
	return b.String()
}

// buildRestoreScript rewinds PATH and managed vars, deleting any that were
// created after the snapshot was taken.
func buildRestoreScript(sn *Snapshot, removeVars []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s v%s — restore snapshot #%d (%s, taken %s)\n",
		appName, appVersion, sn.ID, sn.Scope, sn.TakenAt.Format("2006-01-02 15:04:05"))
	b.WriteString(psPreamble(sn.Scope))

	fmt.Fprintf(&b, "Set-ItemProperty -Path '%s' -Name 'Path' -Value %s -Type ExpandString\n",
		sn.Scope.psPath(), psQuote(sn.Path))

	if len(sn.Vars) > 0 {
		names := make([]string, 0, len(sn.Vars))
		for k := range sn.Vars {
			names = append(names, k)
		}
		sort.Strings(names)
		b.WriteString("\n# Managed variables as they were\n")
		for _, k := range names {
			fmt.Fprintf(&b,
				"Set-ItemProperty -Path '%s' -Name '%s' -Value %s -Type ExpandString\n",
				sn.Scope.psPath(), k, psQuote(sn.Vars[k]))
		}
	}
	if len(removeVars) > 0 {
		b.WriteString("\n# Variables created after this snapshot\n")
		for _, k := range removeVars {
			fmt.Fprintf(&b,
				"Remove-ItemProperty -Path '%s' -Name '%s' -ErrorAction SilentlyContinue\n",
				sn.Scope.psPath(), k)
		}
	}
	b.WriteString("\n")
	b.WriteString(psBroadcast)
	b.WriteString("Write-Host 'Restore complete. Open a new terminal.' -ForegroundColor Green\n")
	return b.String()
}

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
	if rep.Scope == ScopeMachine {
		for _, e := range rep.Entries {
			if !e.Dropped && e.Group == "" &&
				strings.Contains(strings.ToUpper(e.Value), "%USERPROFILE%") {
				w = append(w, "Machine PATH cannot expand %USERPROFILE% / %APPDATA%. "+
					"Move those entries to User scope.")
				break
			}
		}
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
