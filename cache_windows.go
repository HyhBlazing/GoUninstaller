package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type listCacheItem struct {
	Fingerprint string  `json:"fingerprint"`
	App         App     `json:"app"`
	View        viewApp `json:"view"`
}

func listCachePath(tab int) string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "GoUninstaller", fmt.Sprintf("list-cache-%d.json", tab))
}

func appFingerprint(a App) string {
	return strings.Join([]string{
		a.ID, a.Name, a.Version, a.Publisher, a.Command, a.Package, a.Family,
		a.Location, a.IconPath, a.InstalledOn, a.Protection, fmt.Sprintf("%d", a.IconIndex),
		a.QuietCommand, fmt.Sprintf("%t", a.MSI),
	}, "\x1f")
}

func loadListCache(tab int) map[string]listCacheItem {
	data, e := os.ReadFile(listCachePath(tab))
	if e != nil {
		return map[string]listCacheItem{}
	}
	var items []listCacheItem
	if json.Unmarshal(data, &items) != nil {
		return map[string]listCacheItem{}
	}
	result := make(map[string]listCacheItem, len(items))
	for _, item := range items {
		if item.App.ID == "" || item.View.ID == "" {
			continue
		}
		result[item.App.ID] = item
	}
	return result
}

func saveListCache(tab int, items map[string]listCacheItem) {
	list := make([]listCacheItem, 0, len(items))
	for _, item := range items {
		list = append(list, item)
	}
	data, e := json.Marshal(list)
	if e != nil {
		return
	}
	dir := filepath.Dir(listCachePath(tab))
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	_ = os.WriteFile(listCachePath(tab), data, 0600)
}
