package main

import (
	"os"
	"sort"
	"strings"
)

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

// hasUserOnlyToken reports whether any live, ungrouped entry references a
// UserOnly token (%APPDATA%, %LOCALAPPDATA%, %USERPROFILE%) -- those can't
// expand in Machine-scope PATH. Driven by the same token list Tokenize uses,
// so it can't drift out of sync with which tokens are actually UserOnly.
func hasUserOnlyToken(entries []*Entry) bool {
	for _, tk := range detectTokens() {
		if !tk.UserOnly {
			continue
		}
		needle := "%" + strings.ToUpper(tk.Name) + "%"
		for _, e := range entries {
			if e.Dropped || e.Group != "" {
				continue
			}
			if strings.Contains(strings.ToUpper(e.Value), needle) {
				return true
			}
		}
	}
	return false
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
