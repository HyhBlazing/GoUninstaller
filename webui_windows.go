package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"
	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
)

//go:embed ui.html
var uiHTML string

func hasArg(name string) bool {
	for _, a := range os.Args[1:] {
		if a == name {
			return true
		}
	}
	return false
}

// loadUIContent returns HTML and, in --dev mode, the on-disk path for hot reload.
func loadUIContent() (html, path string, err error) {
	if !hasArg("--dev") {
		return uiHTML, "", nil
	}
	candidates := []string{}
	if env := strings.TrimSpace(os.Getenv("GOUNINSTALLER_UI")); env != "" {
		candidates = append(candidates, env)
	}
	if wd, e := os.Getwd(); e == nil {
		candidates = append(candidates, filepath.Join(wd, "ui.html"))
	}
	if exe, e := os.Executable(); e == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "ui.html"))
	}
	seen := map[string]bool{}
	for _, p := range candidates {
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		data, e := os.ReadFile(p)
		if e != nil {
			continue
		}
		fmt.Fprintln(os.Stderr, "dev: 从磁盘加载界面", p)
		return string(data), p, nil
	}
	return "", "", fmt.Errorf("dev 模式未找到 ui.html，可设置环境变量 GOUNINSTALLER_UI 或在项目目录运行")
}

func watchUIHTML(path string, apply func(string)) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	last := info.ModTime()
	for {
		time.Sleep(400 * time.Millisecond)
		info, err = os.Stat(path)
		if err != nil || !info.ModTime().After(last) {
			continue
		}
		last = info.ModTime()
		time.Sleep(120 * time.Millisecond) // debounce save
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "dev: 读取 ui.html 失败:", err)
			continue
		}
		fmt.Fprintln(os.Stderr, "dev: 已热更新 ui.html")
		apply(string(data))
	}
}

type viewApp struct {
	ID, Name, Version, Publisher, InstalledOn, Size, Icon string
	Pinyin, Initials                                      string
	Protection, ProtectionLabel                           string
	SizeBytes                                             uint64
	SizeKnown                                             bool
	Store                                                 bool
}

func toViewApp(a App) viewApp {
	pinyin, initials := searchKeys(a.Name)
	if pub := strings.TrimSpace(a.Publisher); pub != "" {
		pp, pi := searchKeys(pub)
		pinyin += " " + pp
		initials += " " + pi
	}
	protection := a.Protection
	if protection == "" && a.Package != "" {
		protection = classifyStorePackage(a.Family, a.Publisher, -1)
	}
	return viewApp{ID: a.ID, Name: a.Name, Version: a.Version, Publisher: a.Publisher, InstalledOn: a.InstalledOn, Size: formatSize(a), SizeBytes: a.SizeBytes, SizeKnown: a.SizeKnown, Icon: iconPNG(a), Pinyin: pinyin, Initials: initials, Store: a.Package != "", Protection: protection, ProtectionLabel: protectionLabel(protection)}
}

type uiService struct {
	mu         sync.Mutex
	apps       map[string]App
	cache      [2][]App
	loaded     [2]bool
	targets    []App // programs uninstalled in the current single run or batch
	residues   []Residue
	lastExport string
	window     uintptr
	busy       atomic.Bool
	abort      atomic.Bool // set by the UI to stop waiting for an uninstaller
	onProgress func(done, total int)
}

var (
	defaultIconOnce sync.Once
	defaultIconPNG  string
)

func encodeIconPNG(icon *walk.Icon) string {
	if icon == nil {
		return ""
	}
	bmp, e := walk.NewBitmapFromIcon(icon, walk.Size{Width: 32, Height: 32})
	if e != nil || bmp == nil {
		return ""
	}
	defer bmp.Dispose()
	image, e := bmp.ToImage()
	if e != nil {
		return ""
	}
	var buf bytes.Buffer
	if png.Encode(&buf, image) != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func defaultAppIconPNG() string {
	defaultIconOnce.Do(func() {
		defaultIconPNG = encodeIconPNG(walk.IconApplication())
	})
	return defaultIconPNG
}

func iconPNG(a App) string {
	m := &appModel{items: []App{a}}
	defer m.disposeIcons()
	icon, _ := m.Image(0).(*walk.Icon)
	if data := encodeIconPNG(icon); data != "" {
		return data
	}
	if data := defaultAppIconPNG(); data != "" {
		return data
	}
	return encodeIconPNG(walk.IconApplication())
}
func stripVersionFromName(name, version string) string {
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	if name == "" || version == "" {
		return name
	}
	lowerName, lowerVer := strings.ToLower(name), strings.ToLower(version)
	idx := strings.LastIndex(lowerName, lowerVer)
	if idx < 0 {
		return name
	}
	before := strings.TrimRight(name[:idx], " \t-–—vV([（")
	after := strings.TrimLeft(name[idx+len(version):], " \t)]）}")
	cleaned := strings.Join(strings.Fields(strings.TrimSpace(before+" "+after)), " ")
	if cleaned == "" {
		return name
	}
	return cleaned
}

func searchURL(a App, field, engine string) (string, error) {
	value := a.Name
	if field == "publisher" {
		value = a.Publisher
	} else if field != "name" {
		return "", fmt.Errorf("无效的搜索类型")
	} else {
		value = stripVersionFromName(a.Name, a.Version)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("没有可搜索的内容")
	}
	q := url.QueryEscape(value)
	if normalizeSearchEngine(engine) == "baidu" {
		return "https://www.baidu.com/s?wd=" + q, nil
	}
	return "https://www.google.com/search?q=" + q, nil
}
func (s *uiService) app(id string) (App, error) {
	a, ok := s.apps[id]
	if !ok {
		return App{}, fmt.Errorf("程序列表已变更，请刷新后重试")
	}
	return a, nil
}
func (s *uiService) confirm(text string) bool {
	result, _, _ := windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW").Call(s.window, uintptr(unsafe.Pointer(windows.StringToUTF16Ptr(text))), uintptr(unsafe.Pointer(windows.StringToUTF16Ptr("Go Uninstaller"))), 0x4|0x30|0x100)
	return result == 6
}
func (s *uiService) handle(op string, payload string) (interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var p struct {
		Tab               int
		ID, Field, Engine string
		IDs               []string
		Silent, Append    bool
		BatchID           string
		Scope, Format     string
		Query, Status     string
		Page, PageSize    int
		Indices           []int
		Error             string
		Metrics           map[string]float64
	}
	if e := json.Unmarshal([]byte(payload), &p); e != nil {
		return nil, e
	}
	switch op {
	case "getSettings":
		return loadSettings(), nil
	case "saveSettings":
		var cfg Settings
		if e := json.Unmarshal([]byte(payload), &cfg); e != nil {
			return nil, fmt.Errorf("设置格式无效")
		}
		if e := saveSettings(cfg); e != nil {
			return nil, fmt.Errorf("保存设置失败")
		}
		return cfg, nil
	case "list":
		if p.Tab < 0 || p.Tab > 1 {
			return nil, fmt.Errorf("无效标签")
		}
		return s.listTab(p.Tab)
	case "listIgnored":
		return s.listIgnored()
	case "search":
		a, e := s.app(p.ID)
		if e != nil {
			return nil, e
		}
		engine := p.Engine
		if engine == "" {
			engine = loadSettings().SearchEngine
		}
		address, e := searchURL(a, p.Field, engine)
		if e != nil {
			return nil, e
		}
		e = windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(address), nil, nil, 1)
		return nil, e
	case "inspectUninstall":
		out := []uninstallInspection{}
		for _, id := range p.IDs {
			if a, ok := s.apps[id]; ok {
				out = append(out, inspectUninstall(a))
			}
		}
		return out, nil
	case "uninstall":
		// Confirmation is handled in the UI before this call. A batch appends
		// to the same residue session; a single run or a new batch starts one.
		if !p.Append {
			s.targets, s.residues = nil, nil
		}
		a, e := s.app(p.ID)
		if e != nil {
			return nil, e
		}
		s.abort.Store(false)
		result := runUninstall(a, uninstallOptions{Silent: p.Silent, Window: s.window, Abort: &s.abort})
		if result.Removed {
			s.targets = append(s.targets, a)
		}
		appendHistory(uninstallHistoryEntry(a, result, p.BatchID))
		return result, nil
	case "scan":
		return s.scanTargets()
	case "clean":
		return s.cleanResidues(p.Indices)
	case "exportInventory":
		return s.exportInventory(p.Scope, p.Format, p.IDs)
	case "revealExport":
		if s.lastExport == "" {
			return nil, fmt.Errorf("还没有导出的文件")
		}
		return nil, revealInExplorer(s.lastExport)
	case "history":
		return queryHistory(p.Query, p.Status, p.Page, p.PageSize), nil
	case "clearHistory":
		if e := clearHistory(); e != nil {
			return nil, fmt.Errorf("清空日志失败，日志文件可能正被占用")
		}
		appendHistory(HistoryEntry{Action: "clear_history", Target: "操作日志", Status: statusSuccess, Message: "已清空此前的操作日志"})
		return map[string]interface{}{"cleared": true}, nil
	case "exportHistory":
		entries := readHistory()
		if len(entries) == 0 {
			return nil, fmt.Errorf("暂无可导出的日志")
		}
		s.mu.Unlock()
		path, ok, e := saveFileDialog(s.window, "导出操作日志", exportFileName("操作日志", "csv"), "CSV 表格", "csv")
		s.mu.Lock()
		if e != nil {
			return nil, e
		}
		if !ok {
			return map[string]interface{}{"canceled": true}, nil
		}
		if e = writeHistoryCSV(path, entries); e != nil {
			return nil, fmt.Errorf("写入文件失败，请确认目标位置可写")
		}
		s.lastExport = path
		return map[string]interface{}{"path": path, "count": len(entries)}, nil
	case "smokeState":
		return map[string]interface{}{"Loaded": s.loaded, "Desktop": len(s.cache[0]), "Store": len(s.cache[1])}, nil
	}
	return nil, fmt.Errorf("不支持的操作")
}
func (s *uiService) reportProgress(done, total int) {
	if s.onProgress == nil || total <= 0 {
		return
	}
	s.mu.Unlock()
	s.onProgress(done, total)
	s.mu.Lock()
}

func (s *uiService) listTab(tab int) (interface{}, error) {
	var raw []App
	var err error
	if tab == 1 {
		raw, err = storeApps()
	} else {
		raw = desktopApps()
	}
	if err != nil && len(raw) == 0 {
		return nil, err
	}
	disk := loadListCache(tab)
	total := len(raw)
	s.reportProgress(0, total)
	for id, a := range s.apps {
		if (a.Package != "") == (tab == 1) {
			delete(s.apps, id)
		}
	}
	apps := make([]App, 0, total)
	result := make([]viewApp, 0, total)
	next := map[string]listCacheItem{}
	for i, a := range raw {
		fp := appFingerprint(a)
		if cached, ok := disk[a.ID]; ok && cached.Fingerprint == fp && cached.View.ID != "" {
			s.apps[a.ID] = cached.App
			apps = append(apps, cached.App)
			result = append(result, cached.View)
			next[a.ID] = cached
		} else {
			enrichApp(&a)
			view := toViewApp(a)
			s.apps[a.ID] = a
			apps = append(apps, a)
			result = append(result, view)
			next[a.ID] = listCacheItem{Fingerprint: fp, App: a, View: view}
		}
		s.reportProgress(i+1, total)
	}
	saveListCache(tab, next)
	s.cache[tab], s.loaded[tab] = apps, true
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result, err
}

func (s *uiService) listIgnored() (interface{}, error) {
	want := map[string]bool{}
	for _, id := range loadSettings().SystemAppIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return []viewApp{}, nil
	}
	foundApp := map[string]App{}
	foundView := map[string]viewApp{}
	for tab := 0; tab < 2; tab++ {
		if s.loaded[tab] {
			for _, a := range s.cache[tab] {
				if want[a.ID] {
					foundApp[a.ID] = a
				}
			}
		}
		for id, item := range loadListCache(tab) {
			if !want[id] {
				continue
			}
			if _, ok := foundApp[id]; !ok {
				foundApp[id] = item.App
			}
			if item.View.ID != "" {
				foundView[id] = item.View
			}
		}
	}
	needDesktop, needStore := false, false
	for id := range want {
		if _, ok := foundApp[id]; ok {
			continue
		}
		if strings.HasPrefix(id, "desktop:") {
			needDesktop = true
		} else {
			needStore = true
		}
	}
	fetch := func(items []App) {
		for i := range items {
			a := items[i]
			if !want[a.ID] {
				continue
			}
			if _, ok := foundApp[a.ID]; ok {
				continue
			}
			enrichApp(&a)
			foundApp[a.ID] = a
			foundView[a.ID] = toViewApp(a)
		}
	}
	if needDesktop {
		fetch(desktopApps())
	}
	if needStore {
		items, e := storeApps()
		if e != nil && len(foundApp) == 0 {
			return nil, e
		}
		fetch(items)
	}
	result := make([]viewApp, 0, len(want))
	for id := range want {
		a, ok := foundApp[id]
		if !ok {
			continue
		}
		s.apps[a.ID] = a
		if view, ok := foundView[id]; ok && view.ID != "" {
			result = append(result, view)
			continue
		}
		result = append(result, toViewApp(a))
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result, nil
}

// scanTargets looks for residue of every program removed in the current run.
// A program whose install record is still present is never scanned.
func (s *uiService) scanTargets() (interface{}, error) {
	if len(s.targets) == 0 {
		return nil, fmt.Errorf("还没有卸载记录")
	}
	removed := map[string]bool{}
	for _, t := range s.targets {
		removed[t.ID] = true
	}
	// Programs that are still installed protect their own folders from cleanup.
	others := []App{}
	for tab := 0; tab < 2; tab++ {
		for _, a := range s.cache[tab] {
			if !removed[a.ID] {
				others = append(others, a)
			}
		}
	}
	residues := []Residue{}
	seen := map[string]bool{}
	scanned := []string{}
	for _, t := range s.targets {
		installed, e := stillInstalled(t)
		if e != nil || installed {
			continue
		}
		scanned = append(scanned, t.Name)
		for _, r := range scan(t, others) {
			key := r.Kind + "|" + strings.ToLower(r.Path)
			if seen[key] {
				continue
			}
			seen[key] = true
			r.Owner, r.OwnerID = t.Name, t.ID
			residues = append(residues, r)
		}
	}
	if len(scanned) == 0 {
		return map[string]interface{}{"Installed": true}, nil
	}
	s.residues = residues
	return map[string]interface{}{"Items": residues, "Name": strings.Join(scanned, "、"), "Count": len(scanned)}, nil
}

// cleanResidues backs up and removes the chosen candidates, then logs the outcome.
func (s *uiService) cleanResidues(indices []int) (interface{}, error) {
	if len(s.targets) == 0 {
		return nil, fmt.Errorf("没有待清理的卸载记录")
	}
	chosen := map[int]bool{}
	for _, i := range indices {
		if i < 0 || i >= len(s.residues) {
			return nil, fmt.Errorf("残留列表已变更，请关闭对话框后重试")
		}
		chosen[i] = true
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("请勾选要清理的项目")
	}
	// Re-check every owner right before touching anything.
	targets := map[string]App{}
	for _, t := range s.targets {
		targets[t.ID] = t
	}
	verified := map[string]bool{}
	owners := []string{}
	for i := range s.residues {
		if !chosen[i] || verified[s.residues[i].OwnerID] {
			continue
		}
		t, ok := targets[s.residues[i].OwnerID]
		if !ok {
			return nil, fmt.Errorf("残留列表已变更，请关闭对话框后重试")
		}
		if installed, e := stillInstalled(t); e != nil || installed {
			return nil, fmt.Errorf("无法确认「%s」已卸载，已停止清理", t.Name)
		}
		verified[t.ID] = true
		owners = append(owners, t.Name)
	}
	if !s.confirm(fmt.Sprintf("备份并清理选中的 %d 项残留？", len(chosen))) {
		return map[string]interface{}{"Canceled": true}, nil
	}
	started := time.Now()
	backup := filepath.Join(dataDir(), "Backups", started.Format("20060102-150405.000000000"))
	if e := os.MkdirAll(backup, 0700); e != nil {
		return nil, fmt.Errorf("无法创建备份目录，已停止清理")
	}
	remain := []Residue{}
	failures := []string{}
	done := 0
	for i, r := range s.residues {
		if !chosen[i] {
			remain = append(remain, r)
			continue
		}
		r.Selected = true
		if e := backupClean(r, backup); e != nil {
			r.Selected = false
			remain = append(remain, r)
			failures = append(failures, r.Path+": "+e.Error())
		} else {
			done++
		}
	}
	s.residues = remain
	entry := HistoryEntry{Action: "clean", Target: strings.Join(owners, "、"), Status: statusSuccess, DurationMs: time.Since(started).Milliseconds(),
		Message: fmt.Sprintf("已备份并清理 %d 项残留，备份位置 %s", done, backup)}
	if len(failures) > 0 {
		entry.Status, entry.Code = statusFailed, "clean_partial"
		entry.Message = fmt.Sprintf("已清理 %d 项，%d 项失败（可能权限不足或文件被占用）：%s", done, len(failures), strings.Join(failures, "；"))
	}
	appendHistory(entry)
	return map[string]interface{}{"Items": remain, "Done": done, "Backup": backup, "Failures": failures}, nil
}

// exportInventory writes the chosen programs to a file the user picks.
func (s *uiService) exportInventory(scope, format string, ids []string) (interface{}, error) {
	label := "CSV 表格"
	if strings.EqualFold(format, "json") {
		format, label = "json", "JSON 文件"
	} else {
		format = "csv"
	}
	apps := []App{}
	partial := false
	if scope == "all" {
		for tab := 0; tab < 2; tab++ {
			if !s.loaded[tab] {
				if _, e := s.listTab(tab); e != nil && !s.loaded[tab] {
					if tab == 0 {
						return nil, fmt.Errorf("读取程序列表失败，请刷新后重试")
					}
					partial = true
				}
			}
			apps = append(apps, s.cache[tab]...)
		}
		sort.SliceStable(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	} else {
		for _, id := range ids {
			if a, ok := s.apps[id]; ok {
				apps = append(apps, a)
			}
		}
	}
	if len(apps) == 0 {
		return nil, fmt.Errorf("没有可导出的程序，请刷新列表后重试")
	}
	s.mu.Unlock()
	path, ok, e := saveFileDialog(s.window, "导出软件清单", exportFileName("软件清单", format), label, format)
	s.mu.Lock()
	if e != nil {
		return nil, e
	}
	if !ok {
		return map[string]interface{}{"canceled": true}, nil
	}
	entry := HistoryEntry{Action: "export", Target: filepath.Base(path), Status: statusSuccess, Message: fmt.Sprintf("已导出 %d 个程序到 %s", len(apps), path)}
	if e = writeInventory(path, format, apps); e != nil {
		entry.Status, entry.Code, entry.Message = statusFailed, "write_failed", "写入文件失败："+path
		appendHistory(entry)
		return nil, fmt.Errorf("写入文件失败，请确认目标位置可写")
	}
	appendHistory(entry)
	s.lastExport = path
	return map[string]interface{}{"path": path, "count": len(apps), "partial": partial}, nil
}
func runUI() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	html, uiPath, err := loadUIContent()
	if err != nil {
		return err
	}
	title := "Go Uninstaller"
	if uiPath != "" {
		title = "Go Uninstaller [dev]"
	}
	w := webview.NewWithOptions(webview.WebViewOptions{DataPath: filepath.Join(os.Getenv("LOCALAPPDATA"), "GoUninstaller", "WebView2"), AutoFocus: true, WindowOptions: webview.WindowOptions{Title: title, Width: 1080, Height: 740, Center: true}})
	if w == nil {
		return fmt.Errorf("无法创建界面，请确认 Microsoft Edge WebView2 Runtime 已安装")
	}
	defer w.Destroy()
	w.SetSize(800, 480, webview.HintMin)
	service := &uiService{apps: map[string]App{}, window: uintptr(w.Window())}
	service.onProgress = func(done, total int) {
		w.Dispatch(func() {
			w.Eval(fmt.Sprintf("onLoadProgress(%d,%d)", done, total))
		})
	}
	// Keep an active uninstall/backup operation alive when the close button is pressed.
	user32 := windows.NewLazySystemDLL("user32.dll")
	index := ^uintptr(3) // GWLP_WNDPROC = -4 on Windows x64.
	previous, _, _ := user32.NewProc("GetWindowLongPtrW").Call(service.window, index)
	if previous != 0 {
		callback := windows.NewCallback(func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
			if message == 0x10 && service.busy.Load() {
				return 0
			}
			result, _, _ := user32.NewProc("CallWindowProcW").Call(previous, hwnd, uintptr(message), wparam, lparam)
			return result
		})
		user32.NewProc("SetWindowLongPtrW").Call(service.window, index, callback)
	}
	smoke := hasArg("--ui-smoke")
	var smokeErr error
	if e := w.Bind("requestBridge", func(id int, op, payload string) {
		if smoke {
			fmt.Fprintln(os.Stderr, "UI check:", op)
		}
		if op == "smokeResize" && smoke {
			var size struct{ Width, Height int }
			json.Unmarshal([]byte(payload), &size)
			w.Dispatch(func() {
				w.SetSize(size.Width, size.Height, webview.HintNone)
				w.Eval(fmt.Sprintf("receive(%d,null,\"\")", id))
			})
			return
		}
		if op == "smokeDone" && smoke {
			var result struct {
				Error   string
				Metrics map[string]float64
			}
			json.Unmarshal([]byte(payload), &result)
			if result.Error != "" {
				smokeErr = fmt.Errorf("UI verification: %s", result.Error)
			}
			data, _ := json.MarshalIndent(result, "", "  ")
			os.WriteFile(filepath.Join(os.TempDir(), "GoUninstaller-ui-check.json"), data, 0600)
			w.Terminate()
			return
		}
		if op == "cancelWait" {
			// Must work while an uninstall holds the busy flag.
			service.abort.Store(true)
			w.Dispatch(func() { w.Eval(fmt.Sprintf("receive(%d,null,\"\")", id)) })
			return
		}
		if !service.busy.CompareAndSwap(false, true) {
			w.Dispatch(func() { w.Eval(fmt.Sprintf("receive(%d,null,%q)", id, "请等待当前操作完成")) })
			return
		}
		go func() {
			result, e := service.handle(op, payload)
			message := ""
			if e != nil {
				message = e.Error()
			}
			b, err := json.Marshal(result)
			if err != nil {
				b = []byte("null")
				message = err.Error()
			}
			service.busy.Store(false)
			w.Dispatch(func() { w.Eval(fmt.Sprintf("receive(%d,%s,%s)", id, b, jsonString(message))) })
		}()
	}); e != nil {
		return e
	}
	if smoke {
		w.Init("window.__SMOKE__=true;")
		go func() {
			time.Sleep(45 * time.Second)
			w.Dispatch(func() { smokeErr = fmt.Errorf("UI verification timed out"); w.Terminate() })
		}()
	}
	w.SetHtml(html)
	if uiPath != "" {
		go watchUIHTML(uiPath, func(content string) {
			w.Dispatch(func() { w.SetHtml(content) })
		})
	}
	w.Run()
	return smokeErr
}
func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }
