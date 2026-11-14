package main

import (
	"encoding/json"
	"fmt"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

type Residue struct {
	Kind, Path, Reason string
	Owner, OwnerID     string // the uninstalled program this candidate belongs to
	View               uint32
	Selected           bool
}

func normalized(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}
func safeProduct(s string) bool {
	n := normalized(s)
	if len([]rune(n)) < 5 {
		return false
	}
	for _, v := range []string{"windows", "microsoft", "software", "programs", "commonfiles", "applications", "packages", "desktop", "downloads", "documents", "temp", "data", "users", "programdata", "appdata"} {
		if n == v {
			return false
		}
	}
	return true
}
func safePath(path string) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) {
		return fmt.Errorf("不支持相对路径或网络路径")
	}
	protected := []string{os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramData"), os.Getenv("USERPROFILE"), os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Packages")}
	for _, p := range protected {
		if p != "" && strings.EqualFold(filepath.Clean(p), path) {
			return fmt.Errorf("受保护的共享目录")
		}
	}
	win := strings.ToLower(filepath.Clean(os.Getenv("SystemRoot"))) + string(os.PathSeparator)
	if strings.HasPrefix(strings.ToLower(path)+string(os.PathSeparator), win) {
		return fmt.Errorf("Windows 系统目录")
	}
	if filepath.Dir(path) == path || filepath.Dir(path) == filepath.VolumeName(path)+`\` {
		return fmt.Errorf("磁盘根目录或直接子目录不参与清理")
	}
	exe, _ := os.Executable()
	if inside(path, exe) {
		return fmt.Errorf("包含本工具")
	}
	for current := path; ; current = filepath.Dir(current) {
		attrs, e := windows.GetFileAttributes(windows.StringToUTF16Ptr(current))
		if e != nil {
			return e
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("路径包含链接或重解析点")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
func inside(parent, child string) bool {
	r, e := filepath.Rel(parent, child)
	return e == nil && (r == "." || (!strings.HasPrefix(r, ".."+string(os.PathSeparator)) && r != ".." && !filepath.IsAbs(r)))
}
func scan(a App, apps []App) []Residue {
	var result []Residue
	seen := map[string]bool{}
	add := func(path, reason string) {
		path = filepath.Clean(path)
		key := strings.ToLower(path)
		if seen[key] || safePath(path) != nil {
			return
		}
		info, e := os.Stat(path)
		if e != nil || !info.IsDir() {
			return
		}
		for _, other := range apps {
			if other.ID != a.ID && other.Location != "" && (inside(path, other.Location) || inside(other.Location, path)) {
				return
			}
		}
		seen[key] = true
		result = append(result, Residue{Kind: "目录", Path: path, Reason: reason})
	}
	if a.Package != "" {
		add(filepath.Join(os.Getenv("LOCALAPPDATA"), "Packages", a.Family), "当前用户的应用数据目录；可能包含个人数据")
		return result
	}
	// Conservative: exact normalized product names only, no fuzzy/global disk scan.
	if !safeProduct(a.Name) {
		return result
	}
	match := func(s string) bool { return normalized(s) == normalized(a.Name) }
	if a.Location != "" && match(filepath.Base(filepath.Clean(a.Location))) {
		add(a.Location, "安装记录指向的产品目录；需人工核对")
	}
	for _, root := range []string{os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"), os.Getenv("ProgramData"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if root == "" {
			continue
		}
		children, _ := os.ReadDir(root)
		for _, c := range children {
			if c.IsDir() && match(c.Name()) {
				add(filepath.Join(root, c.Name()), "目录名称与软件名称一致；可能包含设置或个人数据")
			}
		}
		if safeProduct(a.Publisher) {
			vendor := filepath.Join(root, a.Publisher)
			children, _ := os.ReadDir(vendor)
			for _, c := range children {
				if c.IsDir() && match(c.Name()) {
					add(filepath.Join(vendor, c.Name()), "发布者下的同名产品目录；需人工核对")
				}
			}
		}
	}
	// Only current-user product keys. Shared publisher keys are never candidates.
	for _, base := range []string{`Software`, `Software\` + a.Publisher} {
		if base != `Software` && !safeProduct(a.Publisher) {
			continue
		}
		for _, view := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
			k, e := registry.OpenKey(registry.CURRENT_USER, base, registry.READ|view)
			if e != nil {
				continue
			}
			names, _ := k.ReadSubKeyNames(-1)
			k.Close()
			for _, name := range names {
				if !match(name) {
					continue
				}
				path := `HKEY_CURRENT_USER\` + base + `\` + name
				key := strings.ToLower(path)
				if !seen[key] {
					seen[key] = true
					result = append(result, Residue{Kind: "注册表", Path: path, Reason: "当前用户下同名产品设置；清理前导出备份", View: view})
				}
			}
		}
	}
	return result
}
func backupClean(r Residue, backup string) error {
	if !r.Selected {
		return nil
	}
	if r.Kind == "目录" {
		if e := safePath(r.Path); e != nil {
			return e
		}
		// Same-volume rename: reversible, never recursively delete or follow links.
		destination := r.Path + ".GoUninstaller-backup-" + time.Now().Format("20060102-150405.000000000")
		record := map[string]string{"original": r.Path, "backup": destination}
		data, _ := json.MarshalIndent(record, "", "  ")
		if e := os.WriteFile(filepath.Join(backup, fmt.Sprintf("folder-%d.json", time.Now().UnixNano())), data, 0600); e != nil {
			return e
		}
		return os.Rename(r.Path, destination)
	}
	if r.Kind != "注册表" || !strings.HasPrefix(r.Path, `HKEY_CURRENT_USER\Software\`) {
		return fmt.Errorf("不允许的清理目标")
	}
	suffix := strings.TrimPrefix(r.Path, `HKEY_CURRENT_USER\`)
	if strings.Count(suffix, `\`) < 1 || strings.Count(suffix, `\`) > 2 {
		return fmt.Errorf("不允许的注册表范围")
	}
	regexe := filepath.Join(os.Getenv("SystemRoot"), "System32", "reg.exe")
	flag := "/reg:64"
	if r.View == registry.WOW64_32KEY {
		flag = "/reg:32"
	}
	file := filepath.Join(backup, fmt.Sprintf("registry-%d.reg", time.Now().UnixNano()))
	cmd := exec.Command(regexe, "export", r.Path, file, "/y", flag)
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	if output, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("备份失败：%v %s", e, output)
	}
	// reg.exe is a native Windows executable, not PowerShell or .NET.
	cmd = exec.Command(regexe, "delete", r.Path, "/f", flag)
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	if output, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("删除失败：%v %s", e, output)
	}
	return nil
}
