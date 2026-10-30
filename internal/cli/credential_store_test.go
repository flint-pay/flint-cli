package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialStoreFreshAndLegacyPresence(t *testing.T) {
	a, _, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_API_KEY", "")
	path, _ := a.configPath()
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	calls := 0
	a.LoadCredential = func(string) (string, error) { calls++; return "", errors.New("locked") }
	if value, _, err := a.resolveCredential("default"); err != nil || value != "" || calls != 0 {
		t.Fatal("fresh profile accessed keyring", err, calls)
	}
	if err := a.saveConfig(Config{DefaultProfile: "default", Profiles: map[string]Profile{"default": {}}}); err != nil {
		t.Fatal(err)
	}
	a.LoadCredential = func(string) (string, error) { calls++; return "", nil }
	for i := 0; i < 2; i++ {
		if _, _, err := a.resolveCredential("default"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("legacy absence looked up %d times", calls)
	}
	cfg, _ := a.loadConfig()
	p := cfg.Profiles["default"]
	if p.CredentialPresent == nil || *p.CredentialPresent || p.CredentialStore != "keyring" {
		t.Fatal("absence not migrated")
	}
	// A legacy saved credential is discovered and marked present.
	cfg.Profiles["saved"] = Profile{}
	if err := a.saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	a.LoadCredential = func(string) (string, error) { return "flint_test_synthetic", nil }
	if value, _, err := a.resolveCredential("saved"); err != nil || value != "flint_test_synthetic" {
		t.Fatal("legacy credential lost", err)
	}
	cfg, _ = a.loadConfig()
	p = cfg.Profiles["saved"]
	if p.CredentialPresent == nil || !*p.CredentialPresent {
		t.Fatal("legacy presence not recorded")
	}
	a.LoadCredential = func(string) (string, error) { return "", errors.New("locked") }
	if _, _, err := a.resolveCredential("saved"); err == nil {
		t.Fatal("configured failure became absence")
	}
}

func TestCredentialStoreFileLifecycleAndPermissions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, authContextJSON("sandbox")) }))
	defer server.Close()
	a, out, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	a.LoadCredential = func(string) (string, error) { t.Error("file backend read keyring"); return "", nil }
	a.StoreCredential = func(string, string) error { t.Error("file backend wrote keyring"); return nil }
	a.DeleteCredential = func(string) error { t.Error("file backend deleted keyring"); return nil }
	for _, key := range []string{"flint_test_first", "flint_test_replacement"} {
		a.Stdin = strings.NewReader(key)
		if exit := a.Run([]string{"auth", "import", "--stdin", "--credential-store", "file", "--output", "json"}); exit != ExitOK {
			t.Fatalf("import exit=%d stderr=%s", exit, stderr)
		}
	}
	path, _ := a.credentialFilePath()
	if runtime.GOOS != "windows" {
		for name, want := range map[string]os.FileMode{filepath.Dir(path): 0700, path: 0600} {
			info, err := os.Stat(name)
			if err != nil || info.Mode().Perm() != want {
				t.Fatalf("permissions %s: %v %v", name, info, err)
			}
		}
	}
	cfg, _ := a.loadConfig()
	p := cfg.Profiles["default"]
	if p.CredentialStore != "file" || p.CredentialPresent == nil || !*p.CredentialPresent {
		t.Fatal("backend/presence not persisted")
	}
	raw, _ := os.ReadFile(filepath.Join(a.ConfigDir, "config.json"))
	if strings.Contains(string(raw), "flint_test_") || strings.Contains(out.String()+stderr.String(), "flint_test_") {
		t.Fatal("secret exposed outside store")
	}
	want := "Credential saved to " + path + " with owner-only permissions. This file is not encrypted by the OS keyring."
	if strings.Count(stderr.String(), want) != 2 {
		t.Fatalf("notice missing: %s", stderr)
	}
	if value, source, err := a.resolveCredential("default"); err != nil || value != "flint_test_replacement" || source != "file" {
		t.Fatal("persisted file store not selected", err, source)
	}
	if exit := a.Run([]string{"auth", "logout", "--confirm", "--output", "json"}); exit != ExitOK {
		t.Fatalf("logout exit=%d stderr=%s", exit, stderr)
	}
	if value, _, err := a.resolveCredential("default"); err != nil || value != "" {
		t.Fatal("logout retained credential", err)
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "flint_test_") {
		t.Fatal("file credential not removed")
	}
}

func TestCredentialStoreSelectionAndConcurrentWrites(t *testing.T) {
	a, _, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_API_KEY", "")
	t.Setenv("FLINT_CREDENTIAL_STORE", "file")
	opts := defaultOptions()
	applyEnvironmentOptions(&opts)
	if opts.CredentialStore != "file" {
		t.Fatal("environment ignored")
	}
	_, opts, _, e := parseInvocation(a.Registry, []string{"version", "--credential-store", "keyring"})
	if e != nil {
		t.Fatal(e)
	}
	applyEnvironmentOptions(&opts)
	if opts.CredentialStore != "keyring" {
		t.Fatal("flag lost precedence")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value := fmt.Sprintf("flint_test_%d", i)
			if _, err := a.fileCredential(fmt.Sprint(i), &value); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for i := 0; i < 8; i++ {
		value, err := a.fileCredential(fmt.Sprint(i), nil)
		if err != nil || value != fmt.Sprintf("flint_test_%d", i) {
			t.Fatal("concurrent write lost", i, err)
		}
	}
}

func TestCredentialStoreFileConcurrentRefresh(t *testing.T) {
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == loginTokenPath {
			refreshes.Add(1)
			writeTestTokens(w, "file-access", "file-refresh")
			return
		}
		fmt.Fprint(w, oauthAuthJSON("sandbox"))
	}))
	defer server.Close()
	a, _, _ := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	a.credentialStore = "file"
	credential := testOAuthCredential(t, server.URL, time.Now().Add(-time.Minute))
	raw, _ := json.Marshal(credential)
	if e := a.saveAuthenticatedCredential("default", string(raw), credential.Auth); e != nil {
		t.Fatal(e)
	}
	a.credentialStore = ""
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := *a
			if c, e := copy.oauthAccess(context.Background(), "default"); e != nil || c.AccessToken != "file-access" {
				t.Error("refresh failed", e)
			}
		}()
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes=%d", refreshes.Load())
	}
	value, _, err := a.resolveCredential("default")
	if err != nil {
		t.Fatal(err)
	}
	c, e := decodeOAuthCredential(value)
	if e != nil || c.RefreshToken != "file-refresh" || c.PendingValidation {
		t.Fatal("rotated credential not committed", e)
	}
}

func TestCredentialStoreFileRollback(t *testing.T) {
	a, _, stderr := testApp(t, "http://127.0.0.1:1")
	a.credentialStore = "file"
	auth := AuthContext{Environment: "sandbox"}
	if e := a.saveAuthenticatedCredential("default", "flint_test_original", auth); e != nil {
		t.Fatal(e)
	}
	stderr.Reset()
	e := func() *CLIError {
		var result *CLIError
		err := a.withConfigLock(func() error {
			result = a.saveCredentialProfileUnlocked("default", "flint_test_new", func(p Profile) Profile {
				path, _ := a.configPath()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				return p
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}()
	if e == nil || e.Code != "CONFIG_WRITE_FAILED" {
		t.Fatal("expected config rollback failure", e)
	}
	// Read directly: the intentionally broken config cannot resolve a profile.
	value, err := a.fileCredential("default", nil)
	if err != nil || value != "flint_test_original" {
		t.Fatal("credential rollback failed", err)
	}
	if strings.Contains(stderr.String(), "Credential saved to") {
		t.Fatal("failed transaction printed success notice")
	}
}

func TestCredentialStoreConfiguredFileFailure(t *testing.T) {
	a, _, _ := testApp(t, "http://127.0.0.1:1")
	t.Setenv("FLINT_API_KEY", "")
	p := a.profileWithCredential("default", Profile{CredentialStore: "file"}, true)
	if err := a.saveConfig(Config{DefaultProfile: "default", Profiles: map[string]Profile{"default": p}}); err != nil {
		t.Fatal(err)
	}
	path, _ := a.credentialFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-json-private-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.resolveCredential("default"); err == nil || strings.Contains(err.Error(), "private-fixture") {
		t.Fatal("configured file failure hidden or file content exposed", err)
	}
}
