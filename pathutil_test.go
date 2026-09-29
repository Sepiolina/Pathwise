package main

import "testing"

func TestHasPathPrefix(t *testing.T) {
	cases := []struct {
		name, s, prefix string
		want            bool
	}{
		{"exact match", `C:\Windows`, `C:\Windows`, true},
		{"boundary respected", `C:\Windows\system32`, `C:\Windows`, true},
		{"false sibling, no boundary", `C:\WindowsApps`, `C:\Windows`, false},
		{"case-insensitive", `c:\windows\system32`, `C:\Windows`, true},
		{"forward-slash boundary counts too", `C:\Windows/system32`, `C:\Windows`, true},
		{"shorter than prefix", `C:\Win`, `C:\Windows`, false},
		{"empty prefix never matches", `C:\Windows`, "", false},
		{"unrelated path", `D:\Data`, `C:\Windows`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasPathPrefix(c.s, c.prefix); got != c.want {
				t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", c.s, c.prefix, got, c.want)
			}
		})
	}
}

func TestNormalizePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{`  C:\Program Files\Git\cmd  `, `C:\Program Files\Git\cmd`},
		{`"C:\Program Files\Git\cmd"`, `C:\Program Files\Git\cmd`},
		{`C:/Program Files/Git/cmd`, `C:\Program Files\Git\cmd`},
		{`C:\Windows\`, `C:\Windows`},
		{`C:\`, `C:\`}, // drive root's trailing slash must survive
		{`C:\Windows\\system32`, `C:\Windows\system32`},
		{`\\server\share\tools\`, `\\server\share\tools`},
		{``, ``},
		{`   `, ``},
	}
	for _, c := range cases {
		if got := normalizePath(c.in); got != c.want {
			t.Errorf("normalizePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExpandWin(t *testing.T) {
	t.Setenv("PATHWISE_TEST_VAR", `C:\Custom`)

	cases := []struct{ in, want string }{
		{`%PATHWISE_TEST_VAR%\bin`, `C:\Custom\bin`},
		{`C:\NoTokensHere`, `C:\NoTokensHere`},
		{`%UNDEFINED_VAR_XYZ%\bin`, `%UNDEFINED_VAR_XYZ%\bin`}, // unknown var passes through
		{`100% sure`, `100% sure`},                             // lone '%' with no closing pair
	}
	for _, c := range cases {
		if got := expandWin(c.in); got != c.want {
			t.Errorf("expandWin(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDedupKey(t *testing.T) {
	t.Setenv("PATHWISE_TEST_VAR", `C:\Custom`)

	a := dedupKey(`"C:\Custom\bin\"`)
	b := dedupKey(`%PATHWISE_TEST_VAR%\bin`)
	if a != b {
		t.Errorf("dedupKey should treat literal and tokenized forms as equal: %q != %q", a, b)
	}

	c := dedupKey(`C:\CUSTOM\BIN`)
	if a != c {
		t.Errorf("dedupKey should be case-insensitive: %q != %q", a, c)
	}
}

func TestBuildPath(t *testing.T) {
	entries := []*Entry{
		{Value: `C:\A`},
		{Value: `C:\B`, Dropped: true, Reason: "duplicate"},
		{Value: `C:\C`, Group: "PATHS_NODE"},
		{Value: `C:\D`},
	}
	got := buildPath(entries, []string{"PATHS_NODE"})
	want := `C:\A` + sep + `C:\D` + sep + `%PATHS_NODE%`
	if got != want {
		t.Errorf("buildPath() = %q, want %q", got, want)
	}
}
