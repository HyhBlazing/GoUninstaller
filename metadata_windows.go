package main

import (
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func installDate(raw string) string {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{"20060102", "2006-01-02", "2006/01/02"} {
		if d, e := time.Parse(layout, raw); e == nil {
			return d.Format("2006-01-02")
		}
	}
	return ""
}

// readInstallDate mirrors Windows “已安装的应用 / 程序和功能”：
// 1) InstallDate (REG_SZ or REG_DWORD yyyymmdd)
// 2) uninstall key last-write time via RegQueryInfoKey
func readInstallDate(k registry.Key) string {
	if s := installDate(regText(k, "InstallDate")); s != "" {
		return s
	}
	if n, _, e := k.GetIntegerValue("InstallDate"); e == nil && n > 0 {
		if s := installDate(fmt.Sprintf("%d", n)); s != "" {
			return s
		}
		if s := installDate(fmt.Sprintf("%08d", n)); s != "" {
			return s
		}
	}
	if info, e := k.Stat(); e == nil {
		t := info.ModTime()
		if !t.IsZero() && t.Year() >= 1990 && t.Before(time.Now().Add(24*time.Hour)) {
			return t.Local().Format("2006-01-02")
		}
	}
	return ""
}

func fileCreatedOn(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return ""
	}
	h, e := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if e != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var created, access, write windows.Filetime
	if windows.GetFileTime(h, &created, &access, &write) != nil {
		return ""
	}
	t := time.Unix(0, created.Nanoseconds())
	if t.IsZero() || t.Year() < 1990 || t.After(time.Now().Add(24*time.Hour)) {
		return ""
	}
	return t.Local().Format("2006-01-02")
}
func parseIconLocation(raw string) (string, int) {
	raw = strings.TrimSpace(raw)
	index := 0
	if i := strings.LastIndex(raw, ","); i >= 0 {
		if n, e := strconv.Atoi(strings.TrimSpace(raw[i+1:])); e == nil {
			index = n
			raw = raw[:i]
		}
	}
	return expandPath(strings.Trim(strings.TrimSpace(raw), `"`)), index
}
func expandPath(path string) string {
	b := make([]uint16, 32768)
	n, e := windows.ExpandEnvironmentStrings(windows.StringToUTF16Ptr(path), &b[0], uint32(len(b)))
	if e == nil && n <= uint32(len(b)) {
		return windows.UTF16ToString(b)
	}
	return path
}
func packagePath(full string) string {
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetPackagePathByFullName")
	var n uint32
	name := windows.StringToUTF16Ptr(full)
	r, _, _ := proc.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&n)), 0)
	if r != 122 || n == 0 || n > 32768 {
		return ""
	}
	buf := make([]uint16, n)
	r, _, _ = proc.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

type appManifest struct {
	Identity struct {
		Name string `xml:"Name,attr"`
	} `xml:"Identity"`
	Properties struct {
		DisplayName string `xml:"DisplayName"`
		Logo        string `xml:"Logo"`
	} `xml:"Properties"`
	Applications []struct {
		ID         string `xml:"Id,attr"`
		Executable string `xml:"Executable,attr"`
		Visual     struct {
			DisplayName string `xml:"DisplayName,attr"`
			Logo        string `xml:"Square44x44Logo,attr"`
		} `xml:"VisualElements"`
	} `xml:"Applications>Application"`
}

func shellAppName(family, id string) string {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := combase.NewProc("RoInitialize").Call(1)
	if hrErr(hr) != nil {
		return ""
	}
	defer combase.NewProc("RoUninitialize").Call()
	guid, _ := windows.GUIDFromString("{43826D1E-E718-42EE-BC55-A1E261C37BFE}")
	var item *comObject
	path := "shell:AppsFolder\\" + family + "!" + id
	hr, _, _ = shell.NewProc("SHCreateItemFromParsingName").Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(path))), 0, uintptr(unsafe.Pointer(&guid)), uintptr(unsafe.Pointer(&item)))
	if hrErr(hr) != nil || item == nil {
		return ""
	}
	defer item.release()
	var pointer uintptr
	if item.call(5, 0, uintptr(unsafe.Pointer(&pointer))) != nil || pointer == 0 {
		return ""
	}
	defer windows.NewLazySystemDLL("ole32.dll").NewProc("CoTaskMemFree").Call(pointer)
	n, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("lstrlenW").Call(pointer)
	if n == 0 || n > 4096 {
		return ""
	}
	buffer := make([]uint16, n)
	var copied uintptr
	if windows.ReadProcessMemory(windows.CurrentProcess(), pointer, (*byte)(unsafe.Pointer(&buffer[0])), n*2, &copied) != nil {
		return ""
	}
	return windows.UTF16ToString(buffer)
}

func resolveResource(full, identity, value string) string {
	if !strings.HasPrefix(strings.ToLower(value), "ms-resource:") {
		return value
	}
	tail := strings.TrimPrefix(value, "ms-resource:")
	var uris []string
	if strings.HasPrefix(tail, "//") {
		uris = []string{value}
	} else if strings.HasPrefix(tail, "/") {
		uris = []string{"ms-resource://" + identity + tail}
	} else {
		uris = []string{"ms-resource://" + identity + "/resources/" + tail, "ms-resource://" + identity + "/" + tail}
	}
	for _, uri := range uris {
		input := "@{" + full + "?" + uri + "}"
		buf := make([]uint16, 4096)
		hr, _, _ := windows.NewLazySystemDLL("shlwapi.dll").NewProc("SHLoadIndirectString").Call(uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(input))), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if int32(hr) >= 0 {
			if s := windows.UTF16ToString(buf); s != "" {
				return s
			}
		}
	}
	return ""
}
func manifestAsset(root, relative string) string {
	relative = strings.ReplaceAll(relative, "/", `\`)
	base := filepath.Join(root, relative)
	if !inside(root, base) {
		return ""
	}
	if s, e := os.Stat(base); e == nil && !s.IsDir() {
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for _, pattern := range []string{stem + ".targetsize-32*" + ext, stem + ".scale-100*" + ext, stem + ".*" + ext} {
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			return matches[0]
		}
	}
	return ""
}
func enrichApp(a *App) {
	if a.Package != "" {
		a.Location = packagePath(a.Package)
		if a.Location != "" {
			if data, e := os.ReadFile(filepath.Join(a.Location, "AppxManifest.xml")); e == nil {
				var manifest appManifest
				if xml.Unmarshal(data, &manifest) == nil {
					display, logo := manifest.Properties.DisplayName, manifest.Properties.Logo
					if len(manifest.Applications) > 0 {
						if v := manifest.Applications[0].Visual.DisplayName; v != "" {
							display = v
						}
						if v := manifest.Applications[0].Visual.Logo; v != "" {
							logo = v
						}
					}
					if name := resolveResource(a.Package, manifest.Identity.Name, display); name != "" {
						a.Name = name
					}
					if len(manifest.Applications) > 0 {
						if name := shellAppName(a.Family, manifest.Applications[0].ID); name != "" {
							a.Name = name
						}
					}
					a.IconPath = manifestAsset(a.Location, logo)
				}
			}
		}
	} else {
		a.Location = strings.Trim(expandPath(a.Location), `"`)
		if a.IconPath == "" {
			if exe, _, err := uninstallCommand(a.Command); err == nil {
				a.IconPath = exe
			}
		}
	}
	if a.InstalledOn == "" && a.Location != "" {
		a.InstalledOn = fileCreatedOn(a.Location)
	}
	if !a.SizeKnown && a.Location != "" {
		if size, ok := boundedDirectorySize(a.Location); ok {
			a.SizeBytes, a.SizeKnown, a.SizeSource = size, true, "安装目录文件合计（不含其他位置的数据）"
		}
	}
}
func boundedDirectorySize(root string) (uint64, bool) {
	if !filepath.IsAbs(root) || filepath.Dir(root) == filepath.VolumeName(root)+`\` {
		return 0, false
	}
	for _, shared := range []string{os.Getenv("SystemRoot"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA"), os.Getenv("APPDATA"), os.Getenv("ProgramData")} {
		if shared != "" && strings.EqualFold(root, shared) {
			return 0, false
		}
	}
	start := time.Now()
	var size uint64
	count := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		count++
		if count > 20000 || time.Since(start) > 150*time.Millisecond {
			return fmt.Errorf("size scan budget exceeded")
		}
		attrs, e := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
		if e != nil {
			return e
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			info, e := d.Info()
			if e != nil {
				return e
			}
			size += uint64(info.Size())
		}
		return nil
	})
	return size, err == nil
}
func formatSize(a App) string {
	if !a.SizeKnown {
		return "—"
	}
	value := float64(a.SizeBytes)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	u := 0
	for value >= 1024 && u < len(units)-1 {
		value /= 1024
		u++
	}
	return fmt.Sprintf("%.1f %s", value, units[u])
}
func (m *appModel) Image(row int) interface{} {
	if m.icons == nil {
		m.icons = map[string]*walk.Icon{}
	}
	a := m.items[row]
	key := fmt.Sprintf("%s,%d", a.IconPath, a.IconIndex)
	if icon, ok := m.icons[key]; ok {
		return icon
	}
	var icon *walk.Icon
	var e error
	const iconSize = 32
	if a.IconPath != "" {
		if strings.EqualFold(filepath.Ext(a.IconPath), ".png") {
			var bmp *walk.Bitmap
			bmp, e = walk.NewBitmapFromFile(a.IconPath)
			if e == nil {
				icon, e = walk.NewIconFromImageWithSize(bmp, walk.Size{Width: iconSize, Height: iconSize})
				bmp.Dispose()
			}
		} else {
			icon, e = walk.NewIconExtractedFromFileWithSize(a.IconPath, a.IconIndex, iconSize)
		}
	}
	if icon == nil || e != nil {
		// Do not cache/dispose the shared system application icon.
		return walk.IconApplication()
	}
	m.icons[key] = icon
	return icon
}
func (m *appModel) disposeIcons() {
	seen := map[*walk.Icon]bool{}
	for _, icon := range m.icons {
		if icon != nil && !seen[icon] {
			icon.Dispose()
			seen[icon] = true
		}
	}
}
