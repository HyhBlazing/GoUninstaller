package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// recordGoneAfter reports the program as installed for the first n checks.
func recordGoneAfter(n int32) func(App) (bool, error) {
	var calls int32
	return func(App) (bool, error) { return atomic.AddInt32(&calls, 1) <= n, nil }
}

func alwaysInstalled(App) (bool, error) { return true, nil }

func TestInterpretOutcome(t *testing.T) {
	cases := []struct {
		installer string
		exit      int64
		removed   bool
		status    string
		code      string
		reboot    bool
	}{
		{installerUnknown, 0, true, statusSuccess, "", false},
		{installerUnknown, 2, true, statusSuccess, "", false}, // record gone wins over a non-zero exit
		{installerMSI, 3010, true, statusReboot, "reboot_required", true},
		{installerMSI, 3010, false, statusReboot, "reboot_required", true},
		{installerMSI, 1641, false, statusReboot, "reboot_required", true},
		{installerMSI, 1602, false, statusCanceled, "wizard_canceled", false},
		{installerMSI, 1618, false, statusFailed, "installer_busy", false},
		{installerMSI, 1605, false, statusFailed, "msi_unknown_product", false},
		{installerMSI, 1603, false, statusFailed, "msi_fatal", false},
		{installerNSIS, 1602, false, statusFailed, "exit_code", false}, // MSI codes only apply to MSI
		{installerUnknown, 0, false, statusPending, "record_remains", false},
		{installerUnknown, -1, false, statusPending, "untracked", false},
		{installerUnknown, 5, false, statusFailed, "access_denied", false},
		{installerUnknown, 1223, false, statusCanceled, "wizard_canceled", false},
		{installerInno, 7, false, statusFailed, "exit_code", false},
	}
	for _, c := range cases {
		status, code, message, reboot := interpretOutcome(c.installer, c.exit, c.removed)
		if status != c.status || code != c.code || reboot != c.reboot || message == "" {
			t.Errorf("%s exit=%d removed=%t: got %s/%s reboot=%t %q", c.installer, c.exit, c.removed, status, code, reboot, message)
		}
	}
}

func TestUninstallCommandVariants(t *testing.T) {
	exe, args, e := uninstallCommand(`MsiExec /X{12345678-1234-1234-1234-123456789012}`)
	if e != nil || !isMSIExec(exe) || !filepath.IsAbs(exe) || len(args) != 1 {
		t.Fatalf("msiexec without extension: %s %v %v", exe, args, e)
	}
	// Unquoted path with spaces that exists on disk.
	dir := filepath.Join(t.TempDir(), "My App")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	real := filepath.Join(dir, "unins000.exe")
	if e := os.WriteFile(real, []byte("stub"), 0600); e != nil {
		t.Fatal(e)
	}
	exe, args, e = uninstallCommand(real + ` /LOG "a b"`)
	if e != nil || exe != real || len(args) != 2 || args[1] != "a b" {
		t.Fatalf("unquoted existing path: %s %v %v", exe, args, e)
	}
	// Unquoted path with spaces that no longer exists must still resolve to
	// the executable, so it is reported as missing rather than unsupported.
	missing := `C:\Program Files\GoUninstaller Missing Test\uninstall.exe`
	exe, args, e = uninstallCommand(missing + ` /S`)
	if e != nil || exe != missing || len(args) != 1 || args[0] != "/S" {
		t.Fatalf("unquoted missing path: %s %v %v", exe, args, e)
	}
	if _, _, e := uninstallCommand(`C:\Tools\remove.bat`); e == nil {
		t.Fatal("non-EXE command accepted")
	}
	if _, _, e := uninstallCommand(`   `); e == nil {
		t.Fatal("empty command accepted")
	}
	if _, _, e := uninstallCommand(`definitely-not-a-system-tool.exe /x`); e == nil {
		t.Fatal("relative executable outside System32 accepted")
	}
}

func TestSilentPlans(t *testing.T) {
	// Windows Installer: any UI level is replaced, removal stays /x.
	plan, e := planUninstall(App{Command: `MsiExec.exe /I{12345678-1234-1234-1234-123456789012} /qb`, MSI: true}, true)
	joined := strings.Join(plan.Args, " ")
	if e != nil || !plan.Silent || plan.Installer != installerMSI || !strings.Contains(joined, "/x{") || !strings.Contains(joined, "/qn") || !strings.Contains(joined, "/norestart") || strings.Contains(joined, "/qb") {
		t.Fatalf("msi silent: %+v %v", plan, e)
	}
	// Standard mode never adds switches.
	plan, e = planUninstall(App{Command: `MsiExec.exe /X{12345678-1234-1234-1234-123456789012}`}, false)
	if e != nil || plan.Silent || len(plan.Args) != 1 {
		t.Fatalf("msi standard: %+v %v", plan, e)
	}
	// Inno Setup, recognised by the _is1 key and by the uninstaller name.
	inno := App{Command: `"C:\Apps\Demo\unins000.exe"`, RegistryPath: uninstallRoot + `\Demo_is1`}
	plan, e = planUninstall(inno, true)
	if e != nil || !plan.Silent || plan.Installer != installerInno || !hasSwitch(plan.Args, "/VERYSILENT") || !hasSwitch(plan.Args, "/SUPPRESSMSGBOXES") || !hasSwitch(plan.Args, "/NORESTART") {
		t.Fatalf("inno silent: %+v %v", plan, e)
	}
	plan, _ = planUninstall(inno, false)
	if plan.Silent || len(plan.Args) != 0 {
		t.Fatalf("inno standard changed the command: %+v", plan)
	}
	// The vendor's QuietUninstallString wins over heuristics.
	vendor := App{Command: `"C:\Apps\Tool\remove.exe"`, QuietCommand: `"C:\Apps\Tool\remove.exe" --quiet --yes`}
	plan, e = planUninstall(vendor, true)
	if e != nil || !plan.Silent || plan.Installer != installerVendor || len(plan.Args) != 2 || plan.Args[0] != "--quiet" {
		t.Fatalf("vendor quiet: %+v %v", plan, e)
	}
	// NSIS, recognised by the stub marker; /S goes first.
	dir := t.TempDir()
	nsis := filepath.Join(dir, "uninst.exe")
	if e := os.WriteFile(nsis, append(make([]byte, 4096), []byte("...Nullsoft.NSIS.exehead...")...), 0600); e != nil {
		t.Fatal(e)
	}
	plan, e = planUninstall(App{Command: `"` + nsis + `" _?=C:\Apps\X`}, true)
	if e != nil || !plan.Silent || plan.Installer != installerNSIS || len(plan.Args) != 2 || plan.Args[0] != "/S" {
		t.Fatalf("nsis silent: %+v %v", plan, e)
	}
	// Squirrel and WiX Burn.
	plan, _ = planUninstall(App{Command: `"C:\Users\me\AppData\Local\Demo\Update.exe" --uninstall`}, true)
	if !plan.Silent || plan.Installer != installerSquirrel || !hasSwitch(plan.Args, "-s") {
		t.Fatalf("squirrel silent: %+v", plan)
	}
	plan, _ = planUninstall(App{Command: `"C:\ProgramData\Package Cache\{abc}\setup.exe" /uninstall`}, true)
	if !plan.Silent || plan.Installer != installerBurn || !hasSwitch(plan.Args, "/quiet") || !hasSwitch(plan.Args, "/norestart") {
		t.Fatalf("burn silent: %+v", plan)
	}
	// Unknown installers fall back to the interactive command, unchanged.
	plain := filepath.Join(dir, "remove.exe")
	os.WriteFile(plain, []byte("plain"), 0600)
	plan, e = planUninstall(App{Command: `"` + plain + `" /remove`}, true)
	if e != nil || plan.Silent || plan.Installer != installerUnknown || len(plan.Args) != 1 || plan.Args[0] != "/remove" {
		t.Fatalf("unknown installer must stay interactive: %+v %v", plan, e)
	}
	if got := inspectUninstall(App{ID: "x", Command: `"` + plain + `" /remove`}); got.SilentSupported {
		t.Fatalf("unknown installer reported as silent: %+v", got)
	}
	if got := inspectUninstall(App{ID: "s", Package: "p", Family: "Vendor.App_zpdnekdrzrea0", Protection: protectUser}); !got.SilentSupported || got.Installer != installerStore {
		t.Fatalf("store app inspection: %+v", got)
	}
}

func TestRunUninstallGuards(t *testing.T) {
	// Protected Store components are refused before anything is launched.
	blocked := App{ID: "b", Name: "Shell", Package: "x", Family: "Microsoft.Windows.ShellExperienceHost_cw5n1h2txyewy", Protection: protectBlocked}
	if r := runUninstall(blocked, uninstallOptions{Installed: alwaysInstalled}); r.Status != statusBlocked || r.Removed {
		t.Fatalf("blocked component: %+v", r)
	}
	// Already removed (for example by a previous queue item): nothing runs.
	gone := App{ID: "g", Name: "Gone", Command: `"C:\GoUninstaller Missing Test\never-run.exe"`}
	r := runUninstall(gone, uninstallOptions{Installed: func(App) (bool, error) { return false, nil }})
	if r.Status != statusSuccess || r.Code != "already_removed" || !r.Removed {
		t.Fatalf("already removed: %+v", r)
	}
	// A registered uninstaller that no longer exists is reported as such.
	r = runUninstall(gone, uninstallOptions{Installed: alwaysInstalled})
	if r.Status != statusFailed || r.Code != "missing_uninstaller" || r.Removed {
		t.Fatalf("missing uninstaller: %+v", r)
	}
	r = runUninstall(App{ID: "u", Name: "Script", Command: `C:\Tools\remove.bat`}, uninstallOptions{Installed: alwaysInstalled})
	if r.Status != statusFailed || r.Code != "unsupported_command" {
		t.Fatalf("unsupported command: %+v", r)
	}
	// Asking for silent on an unknown installer is reported as a fallback.
	plain := filepath.Join(t.TempDir(), "remove.exe")
	os.WriteFile(plain, []byte("not a real program"), 0600)
	r = runUninstall(App{ID: "f", Name: "Plain", Command: `"` + plain + `"`}, uninstallOptions{Silent: true, Hidden: true, Installed: alwaysInstalled, Grace: 200 * time.Millisecond})
	if !r.SilentFallback || r.Silent || r.Removed || r.Status == statusSuccess || !strings.Contains(r.Message, "不支持静默卸载") {
		t.Fatalf("silent fallback: %+v", r)
	}
}

// The following tests run cmd.exe from System32 as a stand-in uninstaller.
// Nothing is installed or removed.

func TestRunUninstallSuccess(t *testing.T) {
	a := App{ID: "ok", Name: "Fake", Command: `cmd.exe /c exit 0`}
	r := runUninstall(a, uninstallOptions{Hidden: true, Installed: recordGoneAfter(1), Grace: 2 * time.Second})
	if r.Status != statusSuccess || !r.Removed || r.ExitCode != 0 || r.Elevation != elevNone || r.Silent {
		t.Fatalf("success run: %+v", r)
	}
}

func TestRunUninstallFailureKeepsRecord(t *testing.T) {
	a := App{ID: "bad", Name: "Fake", Command: `cmd.exe /c exit 7`}
	start := time.Now()
	r := runUninstall(a, uninstallOptions{Hidden: true, Installed: alwaysInstalled, Grace: 5 * time.Second})
	if r.Status != statusFailed || r.Removed || r.ExitCode != 7 || r.Code != "exit_code" {
		t.Fatalf("failed run: %+v", r)
	}
	// A failing exit code without children does not wait out the full grace period.
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("failure waited too long: %s", elapsed)
	}
}

func TestRunUninstallWaitsForChildProcess(t *testing.T) {
	// Like an NSIS stub: the launched process exits at once and a child does the work.
	a := App{ID: "boot", Name: "Bootstrapper", Command: `cmd.exe /c start "" /b cmd.exe /c "ping -n 4 127.0.0.1 >nul"`}
	start := time.Now()
	r := runUninstall(a, uninstallOptions{Hidden: true, Installed: alwaysInstalled, Grace: 200 * time.Millisecond})
	elapsed := time.Since(start)
	if r.Removed || r.Status != statusPending {
		t.Fatalf("bootstrapper outcome: %+v", r)
	}
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("returned after %s without waiting for the child process", elapsed)
	}
}

func TestRunUninstallCanBeAborted(t *testing.T) {
	var abort atomic.Bool
	a := App{ID: "slow", Name: "Slow", Command: `cmd.exe /c "ping -n 30 127.0.0.1 >nul"`}
	go func() {
		time.Sleep(900 * time.Millisecond)
		abort.Store(true)
	}()
	start := time.Now()
	r := runUninstall(a, uninstallOptions{Hidden: true, Installed: alwaysInstalled, Abort: &abort})
	if r.Status != statusCanceled || r.Code != "wait_aborted" || r.Removed {
		t.Fatalf("aborted run: %+v", r)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("abort took %s", elapsed)
	}
}

func TestUninstallHistoryEntry(t *testing.T) {
	a := App{Name: "Demo", Version: "1.2", Publisher: "Acme", Kind: "桌面软件"}
	entry := uninstallHistoryEntry(a, uninstallResult{Status: statusCanceled, Code: "uac_canceled", Elevation: elevCanceled, ExitCode: -1, Message: "已取消管理员权限确认，未执行卸载"}, "batch-1")
	if entry.Action != "uninstall" || entry.Mode != "standard" || entry.Elevation != elevCanceled || entry.ExitCode != nil || entry.BatchID != "batch-1" || entry.Target != "Demo" {
		t.Fatalf("uac entry: %+v", entry)
	}
	entry = uninstallHistoryEntry(a, uninstallResult{Status: statusSuccess, Silent: true, ExitCode: 0, Elevation: elevRequested}, "")
	if entry.Mode != "silent" || entry.ExitCode == nil || *entry.ExitCode != 0 {
		t.Fatalf("silent entry: %+v", entry)
	}
}
