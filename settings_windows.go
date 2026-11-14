package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Settings struct {
	SystemAppIDs []string `json:"system_app_ids"`
	IncludeAll   bool     `json:"include_all"`
	SearchEngine string   `json:"search_engine"` // google | baidu
	// Remembers the "静默卸载" choice in the confirm dialog. Off by default:
	// a standard uninstall shows the program's own wizard.
	SilentUninstall bool            `json:"silent_uninstall"`
	Columns         map[string]bool `json:"columns"`
}

func settingsPath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "GoUninstaller", "settings.json")
}

func normalizeSearchEngine(engine string) string {
	if strings.EqualFold(strings.TrimSpace(engine), "baidu") {
		return "baidu"
	}
	return "google"
}

func defaultSettings() Settings {
	return Settings{
		SystemAppIDs: []string{},
		IncludeAll:   false,
		SearchEngine: "google",
		Columns: map[string]bool{
			"Name": true, "Version": false, "SizeBytes": true, "InstalledOn": true, "Publisher": false,
		},
	}
}

func normalizeSettings(s *Settings) {
	if s.SystemAppIDs == nil {
		s.SystemAppIDs = []string{}
	}
	s.SearchEngine = normalizeSearchEngine(s.SearchEngine)
	def := defaultSettings().Columns
	if s.Columns == nil {
		s.Columns = map[string]bool{}
	}
	for k, v := range def {
		if _, ok := s.Columns[k]; !ok {
			s.Columns[k] = v
		}
	}
	s.Columns["Name"] = true
}

func loadSettings() Settings {
	s := defaultSettings()
	data, e := os.ReadFile(settingsPath())
	if e != nil {
		return s
	}
	if json.Unmarshal(data, &s) != nil {
		return defaultSettings()
	}
	normalizeSettings(&s)
	return s
}

func saveSettings(s Settings) error {
	normalizeSettings(&s)
	if e := os.MkdirAll(filepath.Dir(settingsPath()), 0700); e != nil {
		return e
	}
	data, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return e
	}
	return os.WriteFile(settingsPath(), data, 0600)
}
