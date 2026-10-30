package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const authContextCacheTTL = 5 * time.Minute

type cachedAuthContext struct {
	StoredAt time.Time   `json:"stored_at"`
	Data     AuthContext `json:"data"`
}

var authContextCache sync.Map

func (a *App) observeCachedAuthContext(auth AuthContext) {
	if a.HTTPClient == nil {
		return
	}
	// Cached checks must still inform MCP's live-access guard and result metadata.
	if observer, ok := a.HTTPClient.Transport.(*mcpEnvironmentObserver); ok {
		observer.mu.Lock()
		observer.environment = normalizeEnvironment(auth.Environment)
		observer.mu.Unlock()
	}
}

func (a *App) authContextCachePath(baseURL, key string) string {
	if !strings.HasPrefix(key, "flint_test_") && !strings.HasPrefix(key, "flint_live_") {
		return ""
	}
	dir, err := a.configDirectory()
	if err != nil {
		return ""
	}
	hash := sha256.Sum256([]byte(key + baseURL))
	return filepath.Join(dir, "auth-context-"+hex.EncodeToString(hash[:])+".json")
}

func (a *App) cachedAuthContext(baseURL, key string) (authContextEnvelope, bool) {
	path := a.authContextCachePath(baseURL, key)
	if path == "" {
		return authContextEnvelope{}, false
	}
	valid := func(entry cachedAuthContext) bool {
		age := a.Now().Sub(entry.StoredAt)
		if age < 0 || age >= authContextCacheTTL || entry.Data.AuthType != "api_key" || entry.Data.MerchantID == "" || entry.Data.Environment == "" {
			return false
		}
		if entry.Data.ExpiresAt != nil {
			if expiry, err := time.Parse(time.RFC3339, *entry.Data.ExpiresAt); err == nil && !a.Now().Before(expiry) {
				return false
			}
		}
		return true
	}
	if value, ok := authContextCache.Load(path); ok {
		if entry := value.(cachedAuthContext); valid(entry) {
			return authContextEnvelope{Data: entry.Data, fromCache: true}, true
		}
		authContextCache.Delete(path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return authContextEnvelope{}, false
	}
	var entry cachedAuthContext
	if json.Unmarshal(raw, &entry) != nil || !valid(entry) {
		return authContextEnvelope{}, false
	}
	authContextCache.Store(path, entry)
	return authContextEnvelope{Data: entry.Data, fromCache: true}, true
}

func (a *App) cacheAuthContext(baseURL, key string, envelope authContextEnvelope) {
	path := a.authContextCachePath(baseURL, key)
	if path == "" || envelope.Data.AuthType != "api_key" {
		return
	}
	// Only the typed non-secret context is persisted, never the key or arbitrary
	// response fields. Request IDs identify a request, so they are not cached.
	entry := cachedAuthContext{StoredAt: a.Now(), Data: envelope.Data}
	authContextCache.Store(path, entry)
	_ = writePrivateJSON(path, entry)
}

func (a *App) invalidateAuthContext(baseURL, key string) {
	if path := a.authContextCachePath(baseURL, key); path != "" {
		authContextCache.Delete(path)
		_ = os.Remove(path)
	}
}
