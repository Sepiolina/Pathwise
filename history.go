package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

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

// GC keeps the newest `keep` automatic snapshots per scope and deletes the
// rest. Non-automatic snapshots (pre-apply, post-apply, pre-restore,
// post-restore, manual) are never touched by GC, regardless of age — the
// subquery is restricted to kind = 'auto' so it can't be crowded out by
// unrelated snapshot kinds when counting what's "newest".
func (s *Store) GC(keep int) (int64, error) {
	if keep < 0 {
		keep = 0
	}
	res, err := s.db.Exec(`
		DELETE FROM snapshots
		WHERE kind = 'auto' AND id NOT IN (
		  SELECT id FROM snapshots a
		  WHERE a.scope = snapshots.scope AND a.kind = 'auto'
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
