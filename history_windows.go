package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The operation log is an append-only JSON Lines file. It records what was
// attempted, how elevation went and how it ended; it never stores file contents.
const (
	historyMaxBytes = 2 << 20
	historyKeep     = 2000
)

type HistoryEntry struct {
	ID         string `json:"id"`
	Time       string `json:"time"`
	Action     string `json:"action"` // uninstall | clean | export | clear_history
	Target     string `json:"target"`
	Version    string `json:"version,omitempty"`
	Publisher  string `json:"publisher,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Mode       string `json:"mode,omitempty"` // standard | silent
	Installer  string `json:"installer,omitempty"`
	Status     string `json:"status"`
	Code       string `json:"code,omitempty"`
	Elevation  string `json:"elevation,omitempty"`
	ExitCode   *int64 `json:"exit_code,omitempty"`
	Message    string `json:"message,omitempty"`
	DurationMs int64  `json:"duration_ms,omitempty"`
	BatchID    string `json:"batch_id,omitempty"`
}

type historyPage struct {
	Items    []HistoryEntry `json:"items"`
	Total    int            `json:"total"`     // entries matching the filters
	AllTotal int            `json:"all_total"` // entries in the log
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
}

var (
	historyMu sync.Mutex

	actionLabels    = map[string]string{"uninstall": "卸载", "clean": "清理残留", "export": "导出清单", "clear_history": "清空日志"}
	statusLabels    = map[string]string{statusSuccess: "已完成", statusReboot: "需重启", statusPending: "未完成", statusCanceled: "已取消", statusFailed: "失败", statusBlocked: "已阻止"}
	elevationLabels = map[string]string{elevNone: "无需提权", elevElevated: "管理员身份", elevRequested: "已通过 UAC", elevCanceled: "UAC 已取消", elevFailed: "提权失败"}
	modeLabels      = map[string]string{"standard": "标准", "silent": "静默"}
)

func dataDir() string     { return filepath.Join(os.Getenv("LOCALAPPDATA"), "GoUninstaller") }
func historyPath() string { return filepath.Join(dataDir(), "history.jsonl") }

// appendHistory never fails the calling operation: logging is best effort.
func appendHistory(entry HistoryEntry) {
	historyMu.Lock()
	defer historyMu.Unlock()
	now := time.Now()
	if entry.Time == "" {
		entry.Time = now.Format(time.RFC3339)
	}
	if entry.ID == "" {
		entry.ID = strconv.FormatInt(now.UnixNano(), 36)
	}
	line, e := json.Marshal(entry)
	if e != nil || os.MkdirAll(dataDir(), 0700) != nil {
		return
	}
	f, e := os.OpenFile(historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		return
	}
	f.Write(append(line, '\n'))
	f.Close()
	if info, e := os.Stat(historyPath()); e == nil && info.Size() > historyMaxBytes {
		entries := readHistoryLocked()
		if len(entries) > historyKeep {
			entries = entries[len(entries)-historyKeep:]
		}
		writeHistoryLocked(entries)
	}
}

func readHistoryLocked() []HistoryEntry {
	data, e := os.ReadFile(historyPath())
	if e != nil {
		return []HistoryEntry{}
	}
	lines := bytes.Split(data, []byte{'\n'})
	entries := make([]HistoryEntry, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry HistoryEntry
		// A torn or damaged line must not hide the rest of the log.
		if json.Unmarshal(line, &entry) == nil && entry.Action != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

func writeHistoryLocked(entries []HistoryEntry) error {
	var buf bytes.Buffer
	for _, entry := range entries {
		line, e := json.Marshal(entry)
		if e != nil {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	temp := historyPath() + ".tmp"
	if e := os.WriteFile(temp, buf.Bytes(), 0600); e != nil {
		return e
	}
	return os.Rename(temp, historyPath())
}

// readHistory returns all entries, oldest first.
func readHistory() []HistoryEntry {
	historyMu.Lock()
	defer historyMu.Unlock()
	return readHistoryLocked()
}

func clearHistory() error {
	historyMu.Lock()
	defer historyMu.Unlock()
	if e := os.Remove(historyPath()); e != nil && !os.IsNotExist(e) {
		return e
	}
	return nil
}

// queryHistory filters newest first and returns one page.
func queryHistory(query, status string, page, size int) historyPage {
	all := readHistory()
	if size <= 0 || size > 200 {
		size = 50
	}
	query = strings.ToLower(strings.TrimSpace(query))
	matched := make([]HistoryEntry, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		entry := all[i]
		if status != "" && entry.Status != status {
			continue
		}
		if query != "" {
			hay := strings.ToLower(strings.Join([]string{
				entry.Target, entry.Version, entry.Publisher, entry.Kind, entry.Message,
				actionLabels[entry.Action], statusLabels[entry.Status], elevationLabels[entry.Elevation], modeLabels[entry.Mode],
			}, "\n"))
			if !strings.Contains(hay, query) {
				continue
			}
		}
		matched = append(matched, entry)
	}
	pages := (len(matched) + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	from := (page - 1) * size
	to := from + size
	if to > len(matched) {
		to = len(matched)
	}
	return historyPage{Items: matched[from:to], Total: len(matched), AllTotal: len(all), Page: page, PageSize: size}
}

func uninstallHistoryEntry(a App, r uninstallResult, batch string) HistoryEntry {
	entry := HistoryEntry{
		Action: "uninstall", Target: a.Name, Version: a.Version, Publisher: a.Publisher, Kind: a.Kind,
		Mode: "standard", Installer: r.Installer, Status: r.Status, Code: r.Code, Elevation: r.Elevation,
		Message: r.Message, DurationMs: r.DurationMs, BatchID: batch,
	}
	if r.Silent {
		entry.Mode = "silent"
	}
	if r.ExitCode >= 0 {
		code := r.ExitCode
		entry.ExitCode = &code
	}
	return entry
}

// writeHistoryCSV exports the log newest first for auditing or support.
func writeHistoryCSV(path string, entries []HistoryEntry) error {
	rows := make([][]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		exit, seconds := "", ""
		if entry.ExitCode != nil {
			exit = strconv.FormatInt(*entry.ExitCode, 10)
		}
		if entry.DurationMs > 0 {
			seconds = fmt.Sprintf("%.1f", float64(entry.DurationMs)/1000)
		}
		rows = append(rows, []string{
			strings.Replace(entry.Time, "T", " ", 1), labelOr(actionLabels, entry.Action), entry.Target, entry.Version, entry.Publisher,
			entry.Kind, labelOr(modeLabels, entry.Mode), labelOr(elevationLabels, entry.Elevation), labelOr(statusLabels, entry.Status),
			exit, seconds, entry.Message,
		})
	}
	header := []string{"时间", "操作", "对象", "版本", "发布者", "类型", "方式", "权限", "结果", "退出码", "耗时（秒）", "说明"}
	return writeCSVFile(path, header, rows)
}

func labelOr(labels map[string]string, key string) string {
	if label, ok := labels[key]; ok {
		return label
	}
	return key
}
