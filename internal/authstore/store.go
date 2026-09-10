// Package authstore persists provider accounts as one JSON file each, in the
// same on-disk format the TypeScript build used, so an existing ~/.anti-api/auth
// directory is read without migration.
package authstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gravity-go/internal/paths"
)

// Provider is the credential source. This build ships antigravity only.
const Provider = "antigravity"

// Account is one stored credential set.
type Account struct {
	ID           string         `json:"id"`
	Provider     string         `json:"provider"`
	Email        string         `json:"email,omitempty"`
	Login        string         `json:"login,omitempty"`
	Label        string         `json:"label,omitempty"`
	AccessToken  string         `json:"accessToken"`
	RefreshToken string         `json:"refreshToken,omitempty"`
	ExpiresAt    int64          `json:"expiresAt,omitempty"`
	ProjectID    string         `json:"projectId,omitempty"`
	AuthSource   string         `json:"authSource,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    string         `json:"createdAt,omitempty"`
	UpdatedAt    string         `json:"updatedAt,omitempty"`
}

// Summary is the shape the dashboard's account list expects.
type Summary struct {
	ID          string `json:"id"`
	Provider    string `json:"provider"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email,omitempty"`
	Login       string `json:"login,omitempty"`
	Label       string `json:"label,omitempty"`
	ExpiresAt   int64  `json:"expiresAt,omitempty"`
}

// storedFile is the snake_case on-disk representation.
type storedFile struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Email        string         `json:"email,omitempty"`
	Login        string         `json:"login,omitempty"`
	Label        string         `json:"label,omitempty"`
	AuthSource   string         `json:"auth_source,omitempty"`
	AccessToken  string         `json:"access_token"`
	RefreshToken string         `json:"refresh_token,omitempty"`
	ExpiresAt    int64          `json:"expires_at,omitempty"`
	ProjectID    string         `json:"project_id,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    string         `json:"created_at,omitempty"`
	UpdatedAt    string         `json:"updated_at,omitempty"`
}

const cacheTTL = time.Second

var (
	mu         sync.RWMutex
	cached     []Account
	cacheAt    time.Time
	cacheDirty = true
)

func ensureAuthDir() error { return os.MkdirAll(paths.AuthDir(), 0o755) }

// sanitizeFileKey keeps an account id safe to use as a filename.
func sanitizeFileKey(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		}
		return '_'
	}, v)
}

func fileFor(id string) string {
	return filepath.Join(paths.AuthDir(), Provider+"-"+sanitizeFileKey(id)+".json")
}

func loadFromDisk() []Account {
	if err := ensureAuthDir(); err != nil {
		return nil
	}
	entries, err := os.ReadDir(paths.AuthDir())
	if err != nil {
		return nil
	}
	var out []Account
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(paths.AuthDir(), entry.Name()))
		if err != nil {
			continue
		}
		var raw storedFile
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		// This build only handles antigravity; files from other providers are
		// left on disk untouched and skipped here.
		if raw.Type != Provider || raw.AccessToken == "" || raw.ID == "" {
			continue
		}
		out = append(out, Account{
			ID:           raw.ID,
			Provider:     Provider,
			Email:        raw.Email,
			Login:        raw.Login,
			Label:        raw.Label,
			AccessToken:  raw.AccessToken,
			RefreshToken: raw.RefreshToken,
			ExpiresAt:    raw.ExpiresAt,
			ProjectID:    raw.ProjectID,
			AuthSource:   raw.AuthSource,
			Metadata:     raw.Metadata,
			CreatedAt:    raw.CreatedAt,
			UpdatedAt:    raw.UpdatedAt,
		})
	}
	return out
}

func ensureCacheLocked() []Account {
	expired := time.Since(cacheAt) > cacheTTL
	if cached != nil && !cacheDirty && !expired {
		return cached
	}
	cached = loadFromDisk()
	cacheAt = time.Now()
	cacheDirty = false
	return cached
}

// ResetCacheForTest drops the in-memory cache. The cache has no notion of which
// data directory it was filled from, which is fine in production where that
// never changes, but leaks accounts between tests that each use their own temp
// directory.
func ResetCacheForTest() {
	mu.Lock()
	defer mu.Unlock()
	cached = nil
	cacheDirty = true
}

// List returns every stored antigravity account.
func List() []Account {
	mu.Lock()
	defer mu.Unlock()
	accounts := ensureCacheLocked()
	out := make([]Account, len(accounts))
	copy(out, accounts)
	return out
}

// Summaries renders the account list for the dashboard.
func Summaries() []Summary {
	accounts := List()
	out := make([]Summary, 0, len(accounts))
	for _, a := range accounts {
		display := a.Label
		if display == "" {
			display = a.Email
		}
		if display == "" {
			display = a.Login
		}
		if display == "" {
			display = Provider + "-" + a.ID
		}
		out = append(out, Summary{
			ID:          a.ID,
			Provider:    a.Provider,
			DisplayName: display,
			Email:       a.Email,
			Login:       a.Login,
			Label:       a.Label,
			ExpiresAt:   a.ExpiresAt,
		})
	}
	return out
}

// Get returns one account by id.
func Get(id string) (Account, bool) {
	mu.Lock()
	defer mu.Unlock()
	for _, a := range ensureCacheLocked() {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// Save writes an account to disk and invalidates the cache.
func Save(a Account) error {
	if err := ensureAuthDir(); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	createdAt := a.CreatedAt
	if createdAt == "" {
		createdAt = now
	}
	payload := storedFile{
		ID:           a.ID,
		Type:         Provider,
		Email:        a.Email,
		Login:        a.Login,
		Label:        a.Label,
		AuthSource:   a.AuthSource,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ExpiresAt:    a.ExpiresAt,
		ProjectID:    a.ProjectID,
		Metadata:     a.Metadata,
		CreatedAt:    createdAt,
		UpdatedAt:    now,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(fileFor(a.ID), data, 0o600); err != nil {
		return err
	}
	mu.Lock()
	cacheDirty = true
	mu.Unlock()
	return nil
}

// Delete removes an account file. It reports whether a file was removed.
func Delete(id string) bool {
	path := fileFor(id)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	if err := os.Remove(path); err != nil {
		return false
	}
	mu.Lock()
	cacheDirty = true
	mu.Unlock()
	return true
}
