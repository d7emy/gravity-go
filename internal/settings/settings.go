// Package settings persists the dashboard's user preferences.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gravity-go/internal/paths"
)

// AppSettings mirrors the shape the dashboard reads and writes.
type AppSettings struct {
	Language          string `json:"language"`
	PreloadRouting    bool   `json:"preloadRouting"`
	AutoNgrok         bool   `json:"autoNgrok"`
	AutoOpenDashboard bool   `json:"autoOpenDashboard"`
	AutoRefresh       bool   `json:"autoRefresh"`
	AutoRestart       bool   `json:"autoRestart"`
	PrivacyMode       bool   `json:"privacyMode"`
	CompactLayout     bool   `json:"compactLayout"`
	TrackUsage        bool   `json:"trackUsage"`
	OptimizeQuotaSort bool   `json:"optimizeQuotaSort"`
	CaptureLogs       bool   `json:"captureLogs"`
}

var mu sync.Mutex

func settingsFile() string { return paths.DataFile("settings.json") }

func detectSystemLanguage() string {
	locale := strings.ToLower(strings.Join([]string{
		os.Getenv("LC_ALL"), os.Getenv("LC_MESSAGES"), os.Getenv("LANG"), os.Getenv("LANGUAGE"),
	}, " "))
	if strings.Contains(locale, "zh") {
		return "zh-CN"
	}
	return "en"
}

func defaults() AppSettings {
	return AppSettings{
		Language:          detectSystemLanguage(),
		PreloadRouting:    true,
		AutoNgrok:         false,
		AutoOpenDashboard: true,
		AutoRefresh:       true,
		AutoRestart:       false,
		PrivacyMode:       false,
		CompactLayout:     false,
		TrackUsage:        true,
		OptimizeQuotaSort: false,
		CaptureLogs:       false,
	}
}

// Load reads settings from disk, falling back to defaults.
func Load() AppSettings {
	mu.Lock()
	defer mu.Unlock()
	return loadLocked()
}

func loadLocked() AppSettings {
	out := defaults()
	data, err := os.ReadFile(settingsFile())
	if err != nil {
		return out
	}
	// Unmarshal over the defaults so absent keys keep their default value.
	if err := json.Unmarshal(data, &out); err != nil {
		return defaults()
	}
	return out
}

// Save merges a partial update over the stored settings and writes it back.
// patch holds only the keys the client sent.
func Save(patch map[string]json.RawMessage) (AppSettings, error) {
	mu.Lock()
	defer mu.Unlock()

	current := loadLocked()
	if len(patch) > 0 {
		merged, err := json.Marshal(current)
		if err != nil {
			return current, err
		}
		var asMap map[string]json.RawMessage
		if err := json.Unmarshal(merged, &asMap); err != nil {
			return current, err
		}
		for k, v := range patch {
			asMap[k] = v
		}
		reencoded, err := json.Marshal(asMap)
		if err != nil {
			return current, err
		}
		if err := json.Unmarshal(reencoded, &current); err != nil {
			return current, err
		}
	}

	if _, err := paths.EnsureDataDir(); err != nil {
		return current, err
	}
	payload, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return current, err
	}

	// Write to a temp file then rename, so a crash mid-write cannot truncate
	// the live settings file.
	target := settingsFile()
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return current, err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(target)
		if err2 := os.Rename(tmp, target); err2 != nil {
			return current, err2
		}
	}
	return current, nil
}

// Get returns the whole settings struct (callers read the field they want).
func Get() AppSettings { return Load() }

func init() {
	// Touch the data dir early so the first Save cannot fail on a missing parent.
	_ = os.MkdirAll(filepath.Dir(settingsFile()), 0o755)
}
