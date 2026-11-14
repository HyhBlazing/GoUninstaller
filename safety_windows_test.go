package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lxn/walk"
)

func TestAppFingerprintStable(t *testing.T) {
	a := App{ID: "desktop:true:0:Demo", Name: "Demo", Version: "1.0", Publisher: "Acme", Command: `C:\Demo\unins.exe`, IconPath: `C:\Demo\app.exe`, IconIndex: 0}
	if appFingerprint(a) != appFingerprint(a) {
		t.Fatal("fingerprint unstable")
	}
	b := a
	b.Version = "1.1"
	if appFingerprint(a) == appFingerprint(b) {
		t.Fatal("fingerprint ignored version change")
	}
}
func TestSearchKeysPinyin(t *testing.T) {
	full, initials := searchKeys("微信")
	if full != "weixin" || initials != "wx" {
		t.Fatalf("pinyin keys: %q %q", full, initials)
	}
	full, initials = searchKeys("ABC测试")
	if !strings.Contains(full, "abc") || !strings.HasPrefix(initials, "abc") {
		t.Fatalf("mixed keys: %q %q", full, initials)
	}
}
func TestSettingsNormalize(t *testing.T) {
	s := Settings{Columns: map[string]bool{"Version": true}, SearchEngine: "Baidu"}
	normalizeSettings(&s)
	if !s.Columns["Name"] || s.Columns["Version"] != true || !s.Columns["SizeBytes"] {
		t.Fatalf("normalize columns: %+v", s.Columns)
	}
	if s.SearchEngine != "baidu" {
		t.Fatalf("search engine: %q", s.SearchEngine)
	}
	s.SearchEngine = "unknown"
	normalizeSettings(&s)
	if s.SearchEngine != "google" {
		t.Fatalf("fallback engine: %q", s.SearchEngine)
	}
}
func TestGoogleSearchTerms(t *testing.T) {
	a := App{Name: "Example App + C++ & 中文", Publisher: "Company & Partners"}
	for _, field := range []string{"name", "publisher"} {
		address, e := searchURL(a, field, "google")
		if e != nil {
			t.Fatal(e)
		}
		u, e := url.Parse(address)
		if e != nil {
			t.Fatal(e)
		}
		expected := a.Name
		if field == "publisher" {
			expected = a.Publisher
		}
		if u.Scheme != "https" || u.Host != "www.google.com" || u.Query().Get("q") != expected {
			t.Fatalf("bad search URL: %s", address)
		}
	}
	baidu, e := searchURL(a, "name", "baidu")
	if e != nil {
		t.Fatal(e)
	}
	u, e := url.Parse(baidu)
	if e != nil || u.Host != "www.baidu.com" || u.Query().Get("wd") != a.Name {
		t.Fatalf("bad baidu URL: %s", baidu)
	}
	withVersion := App{Name: "Example App 1.2.3", Version: "1.2.3", Publisher: "Company"}
	address, e := searchURL(withVersion, "name", "")
	if e != nil {
		t.Fatal(e)
	}
	u, e = url.Parse(address)
	if e != nil || u.Query().Get("q") != "Example App" {
		t.Fatalf("version not stripped from name search: %s", address)
	}
	if _, e := searchURL(App{Name: "Example"}, "publisher", "google"); e == nil {
		t.Fatal("empty publisher accepted")
	}
	if _, e := searchURL(a, "command", "google"); e == nil {
		t.Fatal("unexpected search field accepted")
	}
}
func TestWindowsDefaultIcon(t *testing.T) {
	for _, a := range []App{{Name: "Missing icon"}, {Name: "Broken icon", IconPath: `C:\missing-gouninstaller-test\missing.exe`}} {
		data := iconPNG(a)
		if !strings.HasPrefix(data, "data:image/png;base64,") {
			t.Fatal("no default icon")
		}
		decoded, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(data, "data:image/png;base64,"))
		if e != nil {
			t.Fatal(e)
		}
		image, e := png.Decode(bytes.NewReader(decoded))
		if e != nil || image.Bounds().Dx() != 32 || image.Bounds().Dy() != 32 {
			t.Fatal("invalid default icon")
		}
		visible := false
		for y := 0; y < 32; y++ {
			for x := 0; x < 32; x++ {
				_, _, _, alpha := image.At(x, y).RGBA()
				if alpha > 0 {
					visible = true
				}
			}
		}
		if !visible {
			t.Fatal("default icon is transparent")
		}
	}
}

func TestProtectedPaths(t *testing.T) {
	for _, p := range []string{os.Getenv("SystemRoot"), os.Getenv("USERPROFILE"), os.Getenv("APPDATA"), filepath.Join(os.Getenv("SystemRoot"), "System32"), `C:\`, `C:\Users`, `relative\folder`, `\\server\share\data`} {
		if p != "" && safePath(p) == nil {
			t.Errorf("accepted protected path %q", p)
		}
	}
}
func TestNoFuzzyOrSharedMatches(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "Shared")
	if e := os.Mkdir(shared, 0700); e != nil {
		t.Fatal(e)
	}
	a := App{ID: "removed", Name: "Unrelated Product", Location: shared}
	if got := scan(a, nil); len(got) != 0 {
		t.Fatalf("unrelated install path selected: %+v", got)
	}
	a.Name = "Shared"
	other := App{ID: "other", Location: filepath.Join(shared, "OtherApp")}
	if got := scan(a, []App{other}); len(got) != 0 {
		t.Fatalf("shared folder selected: %+v", got)
	}
	for _, n := range []string{"Microsoft", "Windows", "Data", "AppData", "Software"} {
		if safeProduct(n) {
			t.Errorf("accepted broad product %s", n)
		}
	}
}
func TestUncheckedResidueNeverChangesDisk(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "sentinel.txt")
	os.WriteFile(f, []byte("keep"), 0600)
	if e := backupClean(Residue{Kind: "目录", Path: root, Selected: false}, root); e != nil {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(f); e != nil || string(b) != "keep" {
		t.Fatal("unchecked residue changed")
	}
}
func TestDirectoryCleanupIsRecoverable(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "ExampleProduct")
	backup := filepath.Join(root, "records")
	os.Mkdir(target, 0700)
	os.Mkdir(backup, 0700)
	os.WriteFile(filepath.Join(target, "user-data.txt"), []byte("keep"), 0600)
	if e := backupClean(Residue{Kind: "目录", Path: target, Selected: true}, backup); e != nil {
		t.Fatal(e)
	}
	matches, _ := filepath.Glob(target + ".GoUninstaller-backup-*")
	if len(matches) != 1 {
		t.Fatal("missing recoverable folder")
	}
	if b, e := os.ReadFile(filepath.Join(matches[0], "user-data.txt")); e != nil || string(b) != "keep" {
		t.Fatal("backup lost user data")
	}
	if e := os.Rename(matches[0], target); e != nil {
		t.Fatal(e)
	}
}
func TestMSICommandAndQuotedPath(t *testing.T) {
	exe, args, e := uninstallCommand(`MsiExec.exe /I{12345678-1234-1234-1234-123456789012}`)
	if e != nil || !strings.EqualFold(filepath.Base(exe), "msiexec.exe") || len(args) != 1 || !strings.HasPrefix(args[0], "/x{") {
		t.Fatalf("MSI transformation: %s %v %v", exe, args, e)
	}
	exe, args, e = uninstallCommand(`"C:\Program Files\Example\uninstall.exe" /S "a & b"`)
	if e != nil || exe != `C:\Program Files\Example\uninstall.exe` || len(args) != 2 || args[1] != "a & b" {
		t.Fatalf("command quoting: %s %v %v", exe, args, e)
	}
}
func TestSortHandlesEqualValues(t *testing.T) {
	m := &appModel{items: []App{{Name: "B"}, {Name: "A"}, {Name: "A"}}}
	m.Sort(0, walk.SortAscending)
	if m.items[0].Name != "A" || m.items[2].Name != "B" {
		t.Fatal("ascending sort")
	}
	m.Sort(0, walk.SortDescending)
	if m.items[0].Name != "B" || m.items[2].Name != "A" {
		t.Fatal("descending sort")
	}
}

func TestSizeAndDateSort(t *testing.T) {
	m := &appModel{items: []App{{Name: "Small", SizeKnown: true, SizeBytes: 512, InstalledOn: "2025-01-01"}, {Name: "Large", SizeKnown: true, SizeBytes: 1024 * 1024 * 1024, InstalledOn: "2026-02-01"}, {Name: "Unknown"}}}
	m.Sort(2, walk.SortDescending)
	if m.items[0].Name != "Large" || m.items[2].Name != "Unknown" {
		t.Fatal("numeric size sort or unknown placement")
	}
	m.Sort(3, walk.SortAscending)
	if m.items[0].Name != "Small" || m.items[2].Name != "Unknown" {
		t.Fatal("date sort or unknown placement")
	}
	if installDate("20261114") != "2026-11-14" || installDate("invalid") != "" {
		t.Fatal("date parsing")
	}
	if installDate("2026-11-14") != "2026-11-14" || installDate(fmt.Sprintf("%d", 20261114)) != "2026-11-14" {
		t.Fatal("alternate date parsing")
	}
	path, index := parseIconLocation(`"C:\Program Files\Example\app.exe",-101`)
	if path != `C:\Program Files\Example\app.exe` || index != -101 {
		t.Fatal("icon location parsing")
	}
}
