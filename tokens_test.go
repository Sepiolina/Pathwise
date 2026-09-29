package main

import "testing"

// Regression test: the Machine-scope warning tells the user it covers
// %USERPROFILE%, %APPDATA% and %LOCALAPPDATA%, so its detection must
// actually catch all three UserOnly tokens -- not just %USERPROFILE%.
func TestHasUserOnlyToken(t *testing.T) {
	cases := []struct {
		name    string
		entries []*Entry
		want    bool
	}{
		{
			"USERPROFILE token",
			[]*Entry{{Value: `%USERPROFILE%\Baz\bin`}},
			true,
		},
		{
			"APPDATA token (previously missed)",
			[]*Entry{{Value: `%APPDATA%\Bar\bin`}},
			true,
		},
		{
			"LOCALAPPDATA token (previously missed)",
			[]*Entry{{Value: `%LOCALAPPDATA%\Foo\bin`}},
			true,
		},
		{
			"case-insensitive match",
			[]*Entry{{Value: `%appdata%\bar`}},
			true,
		},
		{
			"machine-safe token doesn't count",
			[]*Entry{{Value: `%ProgramFiles%\Git\cmd`}},
			false,
		},
		{
			"plain literal path doesn't count",
			[]*Entry{{Value: `C:\Windows\System32`}},
			false,
		},
		{
			"dropped entries are ignored",
			[]*Entry{{Value: `%APPDATA%\Bar`, Dropped: true, Reason: "duplicate"}},
			false,
		},
		{
			"grouped entries are ignored",
			[]*Entry{{Value: `%APPDATA%\Bar`, Group: "PATHS_NODE"}},
			false,
		},
		{
			"no entries",
			nil,
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasUserOnlyToken(c.entries); got != c.want {
				t.Errorf("hasUserOnlyToken(%v) = %v, want %v", c.entries, got, c.want)
			}
		})
	}
}
