package main

import (
	"encoding/json"
	"fmt"
	"github.com/lxn/walk"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type appModel struct {
	walk.TableModelBase
	walk.SorterBase
	items []App
	icons map[string]*walk.Icon
}

func (m *appModel) RowCount() int { return len(m.items) }
func (m *appModel) Value(row, col int) interface{} {
	a := m.items[row]
	date := a.InstalledOn
	if date == "" {
		date = "—"
	}
	return []string{a.Name, a.Version, formatSize(a), date, a.Publisher}[col]
}
func (m *appModel) Sort(col int, order walk.SortOrder) error {
	sort.SliceStable(m.items, func(i, j int) bool {
		if col == 2 {
			a, b := m.items[i], m.items[j]
			if a.SizeKnown != b.SizeKnown {
				return a.SizeKnown
			}
			if order == walk.SortDescending {
				return a.SizeBytes > b.SizeBytes
			}
			return a.SizeBytes < b.SizeBytes
		}
		if col == 3 {
			a, b := m.items[i].InstalledOn, m.items[j].InstalledOn
			if a == "" || b == "" {
				return a != "" && b == ""
			}
			if order == walk.SortDescending {
				return a > b
			}
			return a < b
		}
		a := strings.ToLower(m.Value(i, col).(string))
		b := strings.ToLower(m.Value(j, col).(string))
		if order == walk.SortDescending {
			return a > b
		}
		return a < b
	})
	m.PublishRowsReset()
	return m.SorterBase.Sort(col, order)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--inventory" {
		apps, e := inventory()
		if e != nil {
			fmt.Fprintln(os.Stderr, "Store inventory:", e)
		}
		json.NewEncoder(os.Stdout).Encode(apps)
		if e != nil {
			os.Exit(1)
		}
		return
	}
	if e := runUI(); e != nil {
		os.MkdirAll(filepath.Join(os.Getenv("LOCALAPPDATA"), "GoUninstaller"), 0700)
		os.WriteFile(filepath.Join(os.Getenv("LOCALAPPDATA"), "GoUninstaller", "startup-error.txt"), []byte(e.Error()), 0600)
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
