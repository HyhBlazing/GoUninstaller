package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var shell = windows.NewLazySystemDLL("shell32.dll")

// Outcome of one uninstall attempt, as shown in the UI and written to the log.
const (
	statusSuccess  = "success"  // install record is gone
	statusReboot   = "reboot"   // finished or pending, Windows restart required
	statusPending  = "pending"  // uninstaller exited cleanly but the record remains
	statusCanceled = "canceled" // user declined UAC, canceled the wizard or stopped waiting
	statusFailed   = "failed"
	statusBlocked  = "blocked" // protected component, never attempted
)

// How the uninstaller obtained (or failed to obtain) administrator rights.
const (
	elevNone      = "none"      // no elevation needed
	elevElevated  = "elevated"  // this tool already runs as administrator
	elevRequested = "requested" // UAC prompt shown and accepted
	elevCanceled  = "canceled"  // UAC prompt declined
	elevFailed    = "failed"    // elevation refused by the system
)

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400

	exitRebootRequired  = 3010
	exitRebootInitiated = 1641
)

type shellExecuteInfo struct {
	Size, Mask                        uint32
	Window                            uintptr
	Verb, File, Parameters, Directory *uint16
	Show                              int32
	Instance, IDList                  uintptr
	Class                             *uint16
	ClassKey                          uintptr
	HotKey                            uint32
	Icon, Process                     uintptr
}

// uninstallError carries a stable code for the log plus a user-facing message.
type uninstallError struct {
	Code    string
	Message string
}

func (e *uninstallError) Error() string { return e.Message }

func failure(code, format string, args ...interface{}) *uninstallError {
	return &uninstallError{Code: code, Message: fmt.Sprintf(format, args...)}
}

func asUninstallError(e error) *uninstallError {
	var ue *uninstallError
	if errors.As(e, &ue) {
		return ue
	}
	return failure("request_failed", "系统未接受卸载请求，请稍后重试")
}

type uninstallResult struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	Removed        bool   `json:"removed"`
	Silent         bool   `json:"silent"`
	SilentFallback bool   `json:"silent_fallback"`
	Installer      string `json:"installer"`
	Elevation      string `json:"elevation"`
	ExitCode       int64  `json:"exit_code"` // -1 when the exit code is unknown
	RebootRequired bool   `json:"reboot_required"`
	DurationMs     int64  `json:"duration_ms"`
}

type uninstallOptions struct {
	Silent    bool
	Window    uintptr                 // owner for the UAC prompt
	Hidden    bool                    // start the uninstaller without a window (tests)
	Abort     *atomic.Bool            // set to stop waiting; the uninstaller is never killed
	Installed func(App) (bool, error) // defaults to stillInstalled
	Grace     time.Duration           // how long to wait for the record to disappear after exit
	MaxWait   time.Duration
}

// runUninstall runs one uninstall end to end and never touches residue.
// Success is decided by the install record disappearing, not by the exit code alone.
func runUninstall(a App, opt uninstallOptions) uninstallResult {
	start := time.Now()
	res := uninstallResult{ID: a.ID, Name: a.Name, ExitCode: -1, Elevation: elevNone, Installer: installerUnknown}
	done := func(status, code, message string) uninstallResult {
		if res.SilentFallback && status != statusSuccess {
			message = "该程序不支持静默卸载，已改用标准卸载。" + message
		}
		res.Status, res.Code, res.Message = status, code, message
		res.DurationMs = time.Since(start).Milliseconds()
		return res
	}
	fail := func(e *uninstallError) uninstallResult {
		status := statusFailed
		if e.Code == "uac_canceled" || e.Code == "wait_aborted" {
			status = statusCanceled
		}
		return done(status, e.Code, e.Message)
	}
	installed := opt.Installed
	if installed == nil {
		installed = stillInstalled
	}
	aborted := func() bool { return opt.Abort != nil && opt.Abort.Load() }

	if e := storeUninstallAllowed(a); e != nil {
		return done(statusBlocked, "protected", e.Error())
	}
	// A previous queue item may already have removed this entry (bundles, suites).
	if still, e := installed(a); e == nil && !still {
		res.Removed = true
		return done(statusSuccess, "already_removed", "安装记录已不存在，无需再次卸载")
	}
	if a.Package != "" {
		res.Installer, res.Silent = installerStore, true
		if e := removeStore(a.Package); e != nil {
			return fail(asUninstallError(e))
		}
		if still, e := installed(a); e != nil || still {
			return done(statusPending, "record_remains", "Windows 已接受卸载请求，但应用仍在列表中，请稍后刷新确认")
		}
		res.Removed = true
		return done(statusSuccess, "", "卸载完成")
	}

	plan, perr := planUninstall(a, opt.Silent)
	if perr != nil {
		return fail(perr)
	}
	res.Installer, res.Silent = plan.Installer, plan.Silent
	res.SilentFallback = opt.Silent && !plan.Silent
	if info, e := os.Stat(plan.Exe); e != nil || info.IsDir() {
		return fail(failure("missing_uninstaller", "卸载程序不存在，安装记录可能已失效，或软件已被手动删除"))
	}
	if plan.Installer == installerMSI && !waitMSIIdle(20*time.Second, aborted) {
		return fail(failure("installer_busy", "另一个 Windows Installer 安装或卸载正在进行，请等待其结束后重试"))
	}
	launchedAt := time.Now()
	proc, lerr := launchProcess(plan.Exe, plan.Args, a.Machine, opt.Window, opt.Hidden)
	res.Elevation = proc.Elevation
	if lerr != nil {
		return fail(lerr)
	}
	exit, removed, werr := waitUninstall(a, proc.Handle, launchedAt, installed, opt, aborted)
	res.ExitCode, res.Removed = exit, removed
	if werr != nil {
		return fail(werr)
	}
	status, code, message, reboot := interpretOutcome(plan.Installer, exit, removed)
	res.RebootRequired = reboot
	return done(status, code, message)
}

type launchedProcess struct {
	Handle    windows.Handle // 0 when Windows did not return a process handle
	Elevation string
}

// launchProcess starts the uninstaller through the shell so UAC can be requested.
func launchProcess(exe string, args []string, elevate bool, hwnd uintptr, hidden bool) (launchedProcess, *uninstallError) {
	selfElevated := windows.GetCurrentProcessToken().IsElevated()
	verb, elevation := "open", elevNone
	if elevate {
		if selfElevated {
			elevation = elevElevated
		} else {
			verb, elevation = "runas", elevRequested
		}
	}
	show := int32(1)
	if hidden {
		show = 0
	}
	params := windows.ComposeCommandLine(args)
	for attempt := 0; ; attempt++ {
		info := shellExecuteInfo{
			Mask:       seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
			Window:     hwnd,
			Verb:       windows.StringToUTF16Ptr(verb),
			File:       windows.StringToUTF16Ptr(exe),
			Parameters: windows.StringToUTF16Ptr(params),
			Show:       show,
		}
		info.Size = uint32(unsafe.Sizeof(info))
		r, _, callErr := shell.NewProc("ShellExecuteExW").Call(uintptr(unsafe.Pointer(&info)))
		if r != 0 {
			h := windows.Handle(info.Process)
			// An uninstaller with a requireAdministrator manifest elevates on "open".
			if elevation == elevNone && !selfElevated && h != 0 && processElevated(h) {
				elevation = elevRequested
			}
			return launchedProcess{Handle: h, Elevation: elevation}, nil
		}
		errno, _ := callErr.(syscall.Errno)
		switch errno {
		case windows.ERROR_CANCELLED:
			return launchedProcess{Elevation: elevCanceled}, failure("uac_canceled", "已取消管理员权限确认，未执行卸载")
		case windows.ERROR_ELEVATION_REQUIRED:
			if verb == "open" && attempt == 0 {
				verb, elevation = "runas", elevRequested
				continue
			}
			return launchedProcess{Elevation: elevFailed}, failure("elevation_failed", "卸载程序需要管理员权限，但系统拒绝了提权请求")
		case windows.ERROR_FILE_NOT_FOUND, windows.ERROR_PATH_NOT_FOUND:
			return launchedProcess{Elevation: elevation}, failure("missing_uninstaller", "卸载程序不存在，安装记录可能已失效，或软件已被手动删除")
		case windows.ERROR_ACCESS_DENIED:
			if verb == "runas" {
				elevation = elevFailed
			}
			return launchedProcess{Elevation: elevation}, failure("access_denied", "没有权限启动卸载程序，可能被系统策略或安全软件拦截")
		case windows.ERROR_BAD_EXE_FORMAT:
			return launchedProcess{Elevation: elevation}, failure("launch_failed", "卸载程序已损坏或与当前系统不兼容，无法运行")
		}
		return launchedProcess{Elevation: elevation}, failure("launch_failed", "无法启动卸载程序（系统错误 %d）", uint32(errno))
	}
}

func processElevated(h windows.Handle) bool {
	var token windows.Token
	if windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token) != nil {
		return false
	}
	defer token.Close()
	return token.IsElevated()
}

// waitMSIIdle waits for a running Windows Installer transaction to finish.
func waitMSIIdle(limit time.Duration, aborted func() bool) bool {
	name, e := windows.UTF16PtrFromString(`Global\_MSIExecute`)
	if e != nil {
		return true
	}
	for end := time.Now().Add(limit); ; {
		h, e := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
		if e != nil {
			return true // not running, or not observable: exit code 1618 still covers it
		}
		windows.CloseHandle(h)
		if aborted() || time.Now().After(end) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitUninstall waits for the uninstaller, any child it handed the work to,
// and then for the install record to settle. It returns the exit code (-1 if
// unknown) and whether the install record disappeared.
func waitUninstall(a App, h windows.Handle, launchedAt time.Time, installed func(App) (bool, error), opt uninstallOptions, aborted func() bool) (int64, bool, *uninstallError) {
	exit := int64(-1)
	maxWait, grace := opt.MaxWait, opt.Grace
	if maxWait <= 0 {
		maxWait = 2 * time.Hour
	}
	if grace <= 0 {
		grace = 6 * time.Second
	}
	deadline := time.Now().Add(maxWait)
	gone := func() bool {
		still, e := installed(a)
		return e == nil && !still
	}
	abortErr := failure("wait_aborted", "已停止等待。卸载程序可能仍在运行，完成后请刷新列表")
	timeoutErr := failure("timeout", "等待卸载程序超时。卸载程序可能仍在运行，完成后请刷新列表")

	if h == 0 {
		// No handle (the request was handed to an already running process):
		// the install record is the only signal available.
		for end := time.Now().Add(45 * time.Second); time.Now().Before(end); {
			if gone() {
				return exit, true, nil
			}
			if aborted() {
				return exit, false, abortErr
			}
			time.Sleep(500 * time.Millisecond)
		}
		return exit, false, nil
	}
	defer windows.CloseHandle(h)
	pid, _ := windows.GetProcessId(h)
	tracker := newProcTracker(pid, launchedAt)
	for {
		event, e := windows.WaitForSingleObject(h, 400)
		if event == windows.WAIT_OBJECT_0 {
			break
		}
		if e != nil {
			return exit, false, failure("wait_failed", "无法跟踪卸载程序的运行状态，完成后请刷新列表")
		}
		tracker.refresh(true)
		if aborted() {
			return exit, false, abortErr
		}
		if time.Now().After(deadline) {
			return exit, false, timeoutErr
		}
	}
	var code uint32
	if windows.GetExitCodeProcess(h, &code) == nil {
		exit = int64(code)
	}
	// Bootstrappers (NSIS, InstallShield, self-copying stubs) exit immediately
	// and leave the real work to a child process.
	for {
		alive := tracker.refresh(false)
		if gone() {
			return exit, true, nil
		}
		if alive == 0 {
			break
		}
		if aborted() {
			return exit, false, abortErr
		}
		if time.Now().After(deadline) {
			return exit, false, timeoutErr
		}
		time.Sleep(400 * time.Millisecond)
	}
	// Some uninstallers drop their record shortly after the last process exits.
	if exit != 0 && exit != exitRebootRequired && exit != exitRebootInitiated && !tracker.sawChild && grace > 1500*time.Millisecond {
		grace = 1500 * time.Millisecond
	}
	for end := time.Now().Add(grace); time.Now().Before(end) && !aborted(); {
		time.Sleep(300 * time.Millisecond)
		if gone() {
			return exit, true, nil
		}
	}
	return exit, false, nil
}

// interpretOutcome turns an exit code and the state of the install record
// into a status, a log code and a message.
func interpretOutcome(installer string, exit int64, removed bool) (status, code, message string, reboot bool) {
	reboot = exit == exitRebootRequired || exit == exitRebootInitiated
	if removed {
		if reboot {
			return statusReboot, "reboot_required", "卸载完成，需要重启 Windows 后完全生效", true
		}
		return statusSuccess, "", "卸载完成", false
	}
	if reboot {
		return statusReboot, "reboot_required", "卸载程序要求重启 Windows 后完成卸载，重启后请刷新列表确认", true
	}
	if installer == installerMSI {
		switch exit {
		case 1602:
			return statusCanceled, "wizard_canceled", "已在卸载向导中取消", false
		case 1605:
			return statusFailed, "msi_unknown_product", "Windows Installer 中找不到该产品，安装记录可能已失效", false
		case 1618:
			return statusFailed, "installer_busy", "另一个 Windows Installer 安装或卸载正在进行，请稍后重试", false
		case 1603:
			return statusFailed, "msi_fatal", "卸载过程中发生严重错误（Windows Installer 1603）", false
		case 1601:
			return statusFailed, "msi_service", "Windows Installer 服务不可用", false
		case 1612, 1619, 1620:
			return statusFailed, "msi_source", "安装源不可用或安装包无法打开，无法完成卸载", false
		case 1625:
			return statusFailed, "msi_policy", "系统策略禁止此卸载操作", false
		}
	}
	switch exit {
	case -1:
		return statusPending, "untracked", "卸载程序已启动，但无法跟踪其运行状态。完成卸载后请刷新列表", false
	case 0:
		return statusPending, "record_remains", "卸载程序已退出，但安装记录仍在。可能已取消卸载，或需要重启后生效", false
	case 5:
		return statusFailed, "access_denied", "卸载程序报告权限不足，未完成卸载", false
	case 1223:
		return statusCanceled, "wizard_canceled", "卸载已被取消", false
	}
	return statusFailed, "exit_code", fmt.Sprintf("卸载未完成（退出码 %d）。可能已在卸载向导中取消，或卸载程序报告了错误", exit), false
}

// uninstallCommand parses a registered uninstall command into an executable
// and arguments without ever invoking a shell.
func uninstallCommand(command string) (string, []string, error) {
	// Expand environment variables without invoking a shell.
	buf := make([]uint16, 32768)
	n, e := windows.ExpandEnvironmentStrings(windows.StringToUTF16Ptr(command), &buf[0], uint32(len(buf)))
	if e != nil || n > uint32(len(buf)) {
		return "", nil, fmt.Errorf("无法展开卸载命令")
	}
	command = strings.TrimSpace(windows.UTF16ToString(buf))
	if command == "" {
		return "", nil, fmt.Errorf("安装记录没有提供卸载命令")
	}
	parts, e := windows.DecomposeCommandLine(command)
	if e != nil || len(parts) == 0 {
		return "", nil, fmt.Errorf("无效的卸载命令")
	}
	// Handle legacy unquoted absolute paths containing spaces by finding the executable.
	if !strings.HasPrefix(command, `"`) {
		if exe, rest, ok := splitUnquotedExe(command); ok {
			parts, e = windows.DecomposeCommandLine(`"` + exe + `"` + rest)
			if e != nil || len(parts) == 0 {
				return "", nil, fmt.Errorf("无效的卸载命令")
			}
		}
	}
	exe, args := parts[0], parts[1:]
	if base := strings.ToLower(filepath.Base(exe)); base == "msiexec" || base == "msiexec.exe" {
		// Always the system copy, and always remove rather than repair/modify.
		exe = filepath.Join(os.Getenv("SystemRoot"), "System32", "msiexec.exe")
		for i, v := range args {
			if strings.EqualFold(v, "/i") || (len(v) > 2 && strings.EqualFold(v[:2], "/i") && v[2] == '{') {
				args[i] = "/x" + v[2:]
			}
		}
		return exe, args, nil
	}
	if !strings.EqualFold(filepath.Ext(exe), ".exe") {
		return "", nil, fmt.Errorf("暂不支持非 EXE 卸载命令，请使用系统设置卸载")
	}
	if !filepath.IsAbs(exe) {
		// Avoid executable lookup through an unsafe working directory.
		candidate := filepath.Join(os.Getenv("SystemRoot"), "System32", exe)
		if _, err := os.Stat(candidate); err != nil {
			return "", nil, fmt.Errorf("卸载程序不是绝对路径：%s", exe)
		}
		exe = candidate
	}
	return exe, args, nil
}

// splitUnquotedExe finds the executable in an unquoted command such as
// `C:\Program Files\App\unins000.exe /SILENT`. It prefers a path that exists
// and otherwise the first absolute candidate, so a missing uninstaller is
// reported as missing instead of as an unsupported command.
func splitUnquotedExe(command string) (exe, rest string, ok bool) {
	lower := strings.ToLower(command)
	first, firstRest := "", ""
	for offset := 0; offset < len(lower); {
		i := strings.Index(lower[offset:], ".exe")
		if i < 0 {
			break
		}
		end := offset + i + 4
		offset = end
		if end < len(command) && command[end] != ' ' && command[end] != '\t' {
			continue
		}
		candidate := strings.TrimSpace(command[:end])
		if !filepath.IsAbs(candidate) {
			continue
		}
		if stat, err := os.Stat(candidate); err == nil && !stat.IsDir() {
			return candidate, command[end:], true
		}
		if first == "" {
			first, firstRest = candidate, command[end:]
		}
	}
	if first != "" && strings.ContainsAny(first, " \t") {
		return first, firstRest, true
	}
	return "", "", false
}
