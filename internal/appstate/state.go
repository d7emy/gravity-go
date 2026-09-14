// Package appstate holds the process-wide auth/runtime state.
//
// The TypeScript original exported a bare mutable object and relied on the JS
// event loop for safety. Go serves handlers concurrently, so every field goes
// behind a mutex and callers read/write through accessors.
package appstate

import "sync"

type snapshot struct {
	AntigravityToken string
	AccessToken      string
	RefreshToken     string
	TokenExpiresAt   int64 // unix millis; 0 = unknown
	UserEmail        string
	UserName         string
	Port             int
	Verbose          bool
	ProjectID        string
	PublicURL        string
}

var (
	mu sync.RWMutex
	s  = snapshot{Port: 8964}
)

// Auth is a consistent read of the credential fields.
type Auth struct {
	AccessToken    string
	RefreshToken   string
	TokenExpiresAt int64
	UserEmail      string
	UserName       string
	ProjectID      string
}

// GetAuth returns the current credential set as one atomic read.
func GetAuth() Auth {
	mu.RLock()
	defer mu.RUnlock()
	return Auth{
		AccessToken:    s.AccessToken,
		RefreshToken:   s.RefreshToken,
		TokenExpiresAt: s.TokenExpiresAt,
		UserEmail:      s.UserEmail,
		UserName:       s.UserName,
		ProjectID:      s.ProjectID,
	}
}

// SetAuth replaces the whole credential set.
func SetAuth(a Auth) {
	mu.Lock()
	defer mu.Unlock()
	s.AccessToken = a.AccessToken
	s.AntigravityToken = a.AccessToken
	s.RefreshToken = a.RefreshToken
	s.TokenExpiresAt = a.TokenExpiresAt
	s.UserEmail = a.UserEmail
	s.UserName = a.UserName
	s.ProjectID = a.ProjectID
}

// ClearAuth wipes every credential field.
func ClearAuth() {
	mu.Lock()
	defer mu.Unlock()
	s.AccessToken = ""
	s.AntigravityToken = ""
	s.RefreshToken = ""
	s.TokenExpiresAt = 0
	s.UserEmail = ""
	s.UserName = ""
	s.ProjectID = ""
}

// SetTokens updates just the access token and its expiry, as a refresh does.
func SetTokens(accessToken string, expiresAtMillis int64) {
	mu.Lock()
	defer mu.Unlock()
	s.AccessToken = accessToken
	s.AntigravityToken = accessToken
	if expiresAtMillis > 0 {
		s.TokenExpiresAt = expiresAtMillis
	}
}

// SetIDEToken records a token read out of the local Antigravity IDE database.
// The IDE stores an apiKey with no refresh token, so expiry stays unknown.
func SetIDEToken(apiKey, email, name string) {
	mu.Lock()
	defer mu.Unlock()
	s.AntigravityToken = apiKey
	s.AccessToken = apiKey
	s.UserEmail = email
	s.UserName = name
}

// AccessToken returns the current bearer token.
func AccessToken() string {
	mu.RLock()
	defer mu.RUnlock()
	return s.AccessToken
}

// IsAuthenticated reports whether any token is loaded.
func IsAuthenticated() bool {
	mu.RLock()
	defer mu.RUnlock()
	return s.AccessToken != ""
}

// ProjectID returns the cloudaicompanion project id.
func ProjectID() string {
	mu.RLock()
	defer mu.RUnlock()
	return s.ProjectID
}

// SetProjectID stores the cloudaicompanion project id.
func SetProjectID(id string) {
	mu.Lock()
	defer mu.Unlock()
	s.ProjectID = id
}

// UserInfo returns the signed-in identity.
func UserInfo() (email, name string) {
	mu.RLock()
	defer mu.RUnlock()
	return s.UserEmail, s.UserName
}

// Port returns the listen port.
func Port() int {
	mu.RLock()
	defer mu.RUnlock()
	return s.Port
}

// SetPort sets the listen port.
func SetPort(p int) {
	mu.Lock()
	defer mu.Unlock()
	s.Port = p
}

// Verbose reports whether debug logging is on.
func Verbose() bool {
	mu.RLock()
	defer mu.RUnlock()
	return s.Verbose
}

// SetVerbose toggles debug logging.
func SetVerbose(v bool) {
	mu.Lock()
	defer mu.Unlock()
	s.Verbose = v
}

// PublicURL returns the active tunnel URL, if any.
func PublicURL() string {
	mu.RLock()
	defer mu.RUnlock()
	return s.PublicURL
}

// SetPublicURL records the active tunnel URL.
func SetPublicURL(u string) {
	mu.Lock()
	defer mu.Unlock()
	s.PublicURL = u
}
