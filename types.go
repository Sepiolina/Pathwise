package main

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
