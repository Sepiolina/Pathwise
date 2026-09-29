package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
)

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
