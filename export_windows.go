package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var comdlg32 = windows.NewLazySystemDLL("comdlg32.dll")

// inventoryRow is one exported program. JSON keys are part of the export format.
type inventoryRow struct {
	Name             string `json:"name"`
	Version          string `json:"version"`
	Publisher        string `json:"publisher"`
	Kind             string `json:"kind"`
	InstalledOn      string `json:"installed_on"`
	SizeBytes        uint64 `json:"size_bytes"`
	SizeKnown        bool   `json:"size_known"`
	SizeText         string `json:"size_text"`
	Scope            string `json:"scope"`
	Architecture     string `json:"architecture"`
	InstallLocation  string `json:"install_location"`
	Protection       string `json:"protection"`
	UninstallCommand string `json:"uninstall_command"`
	Identifier       string `json:"identifier"`
}

type inventoryDocument struct {
	ExportedAt   string         `json:"exported_at"`
	ComputerName string         `json:"computer_name"`
	GeneratedBy  string         `json:"generated_by"`
	Count        int            `json:"count"`
	Items        []inventoryRow `json:"items"`
}

func inventoryRows(apps []App) []inventoryRow {
	rows := make([]inventoryRow, 0, len(apps))
	for _, a := range apps {
		row := inventoryRow{
			Name: a.Name, Version: a.Version, Publisher: a.Publisher, Kind: a.Kind, InstalledOn: a.InstalledOn,
			SizeBytes: a.SizeBytes, SizeKnown: a.SizeKnown, InstallLocation: a.Location, Scope: "当前用户",
		}
		if a.SizeKnown {
			row.SizeText = formatSize(a)
		}
		if a.Package != "" {
			row.Identifier = a.Family
			switch a.Protection {
			case protectBlocked, protectSystem:
				row.Protection = protectionLabel(a.Protection)
			default:
				row.Protection = "普通应用"
			}
		} else {
			row.Identifier, row.UninstallCommand = registryKeyName(a), a.Command
			if a.Machine {
				// Only the machine hive is split into 32/64-bit views.
				row.Scope, row.Architecture = "所有用户", "64 位"
				if a.View == registry.WOW64_32KEY {
					row.Architecture = "32 位"
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// csvSafe neutralises cells a spreadsheet would run as a formula.
func csvSafe(value string) string {
	if value != "" && strings.ContainsRune("=+-@\t\r", rune(value[0])) {
		return "'" + value
	}
	return value
}

// writeCSVFile writes UTF-8 with a BOM and CRLF so Excel opens it directly.
func writeCSVFile(path string, header []string, rows [][]string) error {
	var buf bytes.Buffer
	buf.Write([]byte{0xEF, 0xBB, 0xBF})
	w := csv.NewWriter(&buf)
	w.UseCRLF = true
	if e := w.Write(header); e != nil {
		return e
	}
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, cell := range row {
			safe[i] = csvSafe(cell)
		}
		if e := w.Write(safe); e != nil {
			return e
		}
	}
	w.Flush()
	if e := w.Error(); e != nil {
		return e
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

func writeInventory(path, format string, apps []App) error {
	rows := inventoryRows(apps)
	if format == "json" {
		computer, _ := os.Hostname()
		doc := inventoryDocument{ExportedAt: time.Now().Format(time.RFC3339), ComputerName: computer, GeneratedBy: "Go Uninstaller", Count: len(rows), Items: rows}
		data, e := json.MarshalIndent(doc, "", "  ")
		if e != nil {
			return e
		}
		return os.WriteFile(path, data, 0644)
	}
	header := []string{"名称", "版本", "发布者", "类型", "安装日期", "大小", "大小（字节）", "安装范围", "架构", "安装位置", "保护级别", "卸载命令", "标识"}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		size := ""
		if r.SizeKnown {
			size = strconv.FormatUint(r.SizeBytes, 10)
		}
		out = append(out, []string{r.Name, r.Version, r.Publisher, r.Kind, r.InstalledOn, r.SizeText, size, r.Scope, r.Architecture, r.InstallLocation, r.Protection, r.UninstallCommand, r.Identifier})
	}
	return writeCSVFile(path, header, out)
}

// exportFileName suggests e.g. 软件清单-DESKTOP-20261114-1605.csv.
func exportFileName(prefix, ext string) string {
	computer, _ := os.Hostname()
	computer = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, r) || r < 32 {
			return -1
		}
		return r
	}, computer)
	name := prefix
	if computer != "" {
		name += "-" + computer
	}
	return name + "-" + time.Now().Format("20060102-1504") + "." + ext
}

// OPENFILENAMEW; the layout must match the Windows x64 ABI (152 bytes).
type openFileName struct {
	StructSize      uint32
	Owner           uintptr
	Instance        uintptr
	Filter          *uint16
	CustomFilter    *uint16
	MaxCustomFilter uint32
	FilterIndex     uint32
	File            *uint16
	MaxFile         uint32
	FileTitle       *uint16
	MaxFileTitle    uint32
	InitialDir      *uint16
	Title           *uint16
	Flags           uint32
	FileOffset      uint16
	FileExtension   uint16
	DefExt          *uint16
	CustData        uintptr
	Hook            uintptr
	TemplateName    *uint16
	Reserved        uintptr
	ReservedFlags   uint32
	FlagsEx         uint32
}

// utf16Multi builds a double-NUL terminated string list (dialog filters).
func utf16Multi(parts ...string) []uint16 {
	var out []uint16
	for _, p := range parts {
		out = append(out, utf16.Encode([]rune(p))...)
		out = append(out, 0)
	}
	return append(out, 0)
}

// saveFileDialog shows the native Save dialog. ok is false when the user cancels.
func saveFileDialog(owner uintptr, title, name, label, ext string) (path string, ok bool, err error) {
	// The dialog pumps messages on the calling thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	const (
		ofnOverwritePrompt = 0x00000002
		ofnHideReadOnly    = 0x00000004
		ofnNoChangeDir     = 0x00000008
		ofnPathMustExist   = 0x00000800
		ofnExplorer        = 0x00080000
	)
	file := make([]uint16, 4096)
	encoded := utf16.Encode([]rune(name))
	if len(encoded) >= len(file) {
		encoded = encoded[:len(file)-1]
	}
	copy(file, encoded)
	filter := utf16Multi(label+" (*."+ext+")", "*."+ext, "所有文件 (*.*)", "*.*")
	ofn := openFileName{
		Owner:   owner,
		Filter:  &filter[0],
		File:    &file[0],
		MaxFile: uint32(len(file)),
		Title:   windows.StringToUTF16Ptr(title),
		DefExt:  windows.StringToUTF16Ptr(ext),
		Flags:   ofnOverwritePrompt | ofnHideReadOnly | ofnNoChangeDir | ofnPathMustExist | ofnExplorer,
	}
	ofn.StructSize = uint32(unsafe.Sizeof(ofn))
	r, _, _ := comdlg32.NewProc("GetSaveFileNameW").Call(uintptr(unsafe.Pointer(&ofn)))
	runtime.KeepAlive(filter)
	if r == 0 {
		if code, _, _ := comdlg32.NewProc("CommDlgExtendedError").Call(); code != 0 {
			return "", false, fmt.Errorf("无法打开保存对话框")
		}
		return "", false, nil
	}
	return windows.UTF16ToString(file), true, nil
}

// revealInExplorer opens the containing folder with the file selected.
func revealInExplorer(path string) error {
	if info, e := os.Stat(path); e != nil || info.IsDir() || strings.Contains(path, `"`) {
		return fmt.Errorf("文件已不存在")
	}
	explorer := filepath.Join(os.Getenv("SystemRoot"), "explorer.exe")
	cmd := exec.Command(explorer)
	// explorer.exe needs `/select,"path"` verbatim; default quoting breaks it.
	cmd.SysProcAttr = &windows.SysProcAttr{CmdLine: `"` + explorer + `" /select,"` + path + `"`}
	if e := cmd.Start(); e != nil {
		return fmt.Errorf("无法打开所在文件夹")
	}
	go cmd.Wait()
	return nil
}
