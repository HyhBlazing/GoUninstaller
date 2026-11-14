package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// isolateData points the tool's data directory at a temp folder.
func isolateData(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	return dir
}

func TestHistoryAppendQueryClear(t *testing.T) {
	isolateData(t)
	if page := queryHistory("", "", 1, 50); page.Total != 0 || page.AllTotal != 0 || page.Items == nil || page.Page != 1 {
		t.Fatalf("empty log: %+v", page)
	}
	appendHistory(HistoryEntry{Action: "uninstall", Target: "Alpha Editor", Publisher: "Acme", Status: statusSuccess, Elevation: elevRequested, Mode: "standard"})
	appendHistory(HistoryEntry{Action: "uninstall", Target: "Beta Player", Status: statusCanceled, Code: "uac_canceled", Elevation: elevCanceled, Message: "已取消管理员权限确认，未执行卸载"})
	appendHistory(HistoryEntry{Action: "clean", Target: "Alpha Editor", Status: statusFailed, Message: "已清理 1 项，1 项失败"})

	page := queryHistory("", "", 1, 50)
	if page.Total != 3 || page.AllTotal != 3 || len(page.Items) != 3 {
		t.Fatalf("all entries: %+v", page)
	}
	if page.Items[0].Action != "clean" || page.Items[2].Target != "Alpha Editor" || page.Items[0].ID == "" || page.Items[0].Time == "" {
		t.Fatalf("not newest first or missing id/time: %+v", page.Items)
	}
	if got := queryHistory("", statusCanceled, 1, 50); got.Total != 1 || got.Items[0].Target != "Beta Player" || got.AllTotal != 3 {
		t.Fatalf("status filter: %+v", got)
	}
	// Elevation is searchable through its label: every UAC-related entry,
	// or only the declined prompts.
	if got := queryHistory("uac", "", 1, 50); got.Total != 2 {
		t.Fatalf("UAC keyword: %+v", got)
	}
	if got := queryHistory("UAC 已取消", "", 1, 50); got.Total != 1 || got.Items[0].Code != "uac_canceled" {
		t.Fatalf("declined UAC keyword: %+v", got)
	}
	if got := queryHistory("  ALPHA ", "", 1, 50); got.Total != 2 {
		t.Fatalf("case-insensitive keyword: %+v", got)
	}
	if got := queryHistory("no-such-program", "", 1, 50); got.Total != 0 || got.Items == nil || got.AllTotal != 3 {
		t.Fatalf("no match: %+v", got)
	}
	// Paging clamps out-of-range pages instead of returning nothing.
	if got := queryHistory("", "", 2, 2); got.Page != 2 || len(got.Items) != 1 || got.Total != 3 {
		t.Fatalf("second page: %+v", got)
	}
	if got := queryHistory("", "", 99, 2); got.Page != 2 || len(got.Items) != 1 {
		t.Fatalf("page clamp: %+v", got)
	}
	if got := queryHistory("", "", 0, 0); got.Page != 1 || got.PageSize != 50 {
		t.Fatalf("defaults: %+v", got)
	}
	if e := clearHistory(); e != nil {
		t.Fatal(e)
	}
	if e := clearHistory(); e != nil {
		t.Fatalf("clearing an empty log must succeed: %v", e)
	}
	if got := queryHistory("", "", 1, 50); got.AllTotal != 0 {
		t.Fatalf("log not cleared: %+v", got)
	}
}

func TestHistorySurvivesDamagedLines(t *testing.T) {
	isolateData(t)
	appendHistory(HistoryEntry{Action: "uninstall", Target: "Before", Status: statusSuccess})
	f, e := os.OpenFile(historyPath(), os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	f.WriteString("{\"action\":\"uninstall\",\"target\":\"torn\n\nnot json at all\n")
	f.Close()
	appendHistory(HistoryEntry{Action: "uninstall", Target: "After", Status: statusFailed})
	entries := readHistory()
	if len(entries) != 2 || entries[0].Target != "Before" || entries[1].Target != "After" {
		t.Fatalf("damaged lines hid entries: %+v", entries)
	}
}

func TestHistoryIsTrimmed(t *testing.T) {
	isolateData(t)
	if e := os.MkdirAll(dataDir(), 0700); e != nil {
		t.Fatal(e)
	}
	// Seed a log that is over the size limit, then append once more.
	var buf bytes.Buffer
	filler := strings.Repeat("x", 900)
	for i := 0; i < historyKeep+600; i++ {
		line, _ := json.Marshal(HistoryEntry{ID: fmt.Sprint(i), Time: "2026-01-01T00:00:00+08:00", Action: "uninstall", Target: fmt.Sprintf("App %d", i), Status: statusSuccess, Message: filler})
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if buf.Len() <= historyMaxBytes {
		t.Fatalf("seed too small: %d", buf.Len())
	}
	if e := os.WriteFile(historyPath(), buf.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	appendHistory(HistoryEntry{Action: "uninstall", Target: "Newest", Status: statusSuccess})
	entries := readHistory()
	if len(entries) != historyKeep || entries[len(entries)-1].Target != "Newest" {
		t.Fatalf("trim kept %d entries, last %q", len(entries), entries[len(entries)-1].Target)
	}
	if _, e := os.Stat(historyPath() + ".tmp"); !os.IsNotExist(e) {
		t.Fatal("temp file left behind")
	}
}

func TestHistoryCSVExport(t *testing.T) {
	dir := isolateData(t)
	code := int64(1602)
	entries := []HistoryEntry{
		{Time: "2026-11-14T16:00:00+08:00", Action: "uninstall", Target: "Old", Status: statusSuccess, Mode: "silent", Elevation: elevRequested},
		{Time: "2026-11-14T16:05:00+08:00", Action: "uninstall", Target: "=cmd|' /C calc'!A0", Status: statusCanceled, Mode: "standard", Elevation: elevCanceled, ExitCode: &code, DurationMs: 2500, Message: "已在卸载向导中取消"},
	}
	path := filepath.Join(dir, "log.csv")
	if e := writeHistoryCSV(path, entries); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	text := string(data)
	if !bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) || !strings.Contains(text, "时间,操作,对象") {
		t.Fatalf("missing BOM or header: %q", text[:40])
	}
	lines := strings.Split(strings.TrimSpace(text), "\r\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "UAC 已取消") || !strings.Contains(lines[1], "1602") || !strings.Contains(lines[1], "2.5") || !strings.Contains(lines[2], "静默") {
		t.Fatalf("rows not newest first or labels missing: %q", lines)
	}
	if strings.Contains(text, ",=cmd") || !strings.Contains(text, "'=cmd") {
		t.Fatalf("formula cell not neutralised: %q", lines[1])
	}
}

func TestInventoryExport(t *testing.T) {
	dir := t.TempDir()
	apps := []App{
		{ID: "desktop:true:512:{GUID-1}", Name: "Alpha, Editor", Version: "1.0", Publisher: "Acme", Kind: "桌面软件", Machine: true, View: registry.WOW64_32KEY,
			RegistryPath: uninstallRoot + `\{GUID-1}`, Command: `MsiExec.exe /X{GUID-1}`, Location: `C:\Program Files (x86)\Alpha`, InstalledOn: "2026-01-02", SizeBytes: 2048, SizeKnown: true},
		{ID: "desktop:false:256:Beta", Name: "+Beta", Kind: "桌面软件", RegistryPath: uninstallRoot + `\Beta`, Command: `"C:\Beta\unins000.exe"`},
		{ID: "store:pkg", Name: "Gamma", Kind: "商店应用", Package: "pkg", Family: "Vendor.Gamma_abc", Protection: protectSystem},
	}
	csvPath := filepath.Join(dir, "list.csv")
	if e := writeInventory(csvPath, "csv", apps); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(csvPath)
	text := string(data)
	lines := strings.Split(strings.TrimSpace(text), "\r\n")
	if !bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) || len(lines) != 4 || !strings.Contains(lines[0], "名称,版本,发布者") {
		t.Fatalf("csv shape: %q", lines)
	}
	if !strings.Contains(lines[1], `"Alpha, Editor"`) || !strings.Contains(lines[1], "所有用户") || !strings.Contains(lines[1], "32 位") || !strings.Contains(lines[1], "2048") || !strings.Contains(lines[1], "{GUID-1}") {
		t.Fatalf("machine row: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "'+Beta,") || !strings.Contains(lines[2], "当前用户") || strings.Contains(lines[2], "位") {
		t.Fatalf("user row (formula guard, no architecture, no size): %q", lines[2])
	}
	if !strings.Contains(lines[3], "系统相关") || !strings.Contains(lines[3], "Vendor.Gamma_abc") {
		t.Fatalf("store row: %q", lines[3])
	}

	jsonPath := filepath.Join(dir, "list.json")
	if e := writeInventory(jsonPath, "json", apps); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(jsonPath)
	var doc struct {
		ExportedAt string                   `json:"exported_at"`
		Count      int                      `json:"count"`
		Items      []map[string]interface{} `json:"items"`
	}
	if e := json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	if doc.Count != 3 || len(doc.Items) != 3 || doc.ExportedAt == "" {
		t.Fatalf("json document: %+v", doc)
	}
	for _, key := range []string{"name", "version", "publisher", "installed_on", "size_bytes", "install_location", "uninstall_command", "identifier"} {
		if _, ok := doc.Items[0][key]; !ok {
			t.Errorf("json item is missing snake_case key %q", key)
		}
	}
	if doc.Items[0]["name"] != "Alpha, Editor" || doc.Items[2]["identifier"] != "Vendor.Gamma_abc" {
		t.Fatalf("json values: %+v", doc.Items)
	}
}

func TestExportHelpers(t *testing.T) {
	for in, want := range map[string]string{"=1+1": "'=1+1", "+x": "'+x", "-x": "'-x", "@x": "'@x", "plain": "plain", "": "", "1.2.3": "1.2.3"} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
	name := exportFileName("软件清单", "csv")
	if !strings.HasPrefix(name, "软件清单-") || !strings.HasSuffix(name, ".csv") || strings.ContainsAny(name, `\/:*?"<>|`) {
		t.Fatalf("export file name: %q", name)
	}
	// The save dialog is called through a raw ABI struct.
	if size := unsafe.Sizeof(openFileName{}); size != 152 {
		t.Fatalf("OPENFILENAMEW must be 152 bytes on x64, got %d", size)
	}
	if got := utf16Multi("a", "b"); len(got) != 5 || got[1] != 0 || got[3] != 0 || got[4] != 0 {
		t.Fatalf("filter list not double-NUL terminated: %v", got)
	}
	if e := revealInExplorer(filepath.Join(t.TempDir(), "missing.csv")); e == nil {
		t.Fatal("revealed a file that does not exist")
	}
}
