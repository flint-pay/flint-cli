package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

const keyringService = "flint-cli"

type Config struct {
	DefaultProfile string             `json:"default_profile"`
	Profiles       map[string]Profile `json:"profiles"`
}

type Profile struct {
	CredentialStore   string           `json:"credential_store,omitempty"`
	CredentialPresent *bool            `json:"credential_present,omitempty"`
	PendingSignup     *SignupChallenge `json:"pending_signup,omitempty"`
	ContextID         string           `json:"context,omitempty"`
	MerchantGuard     string           `json:"merchant_guard,omitempty"`
	Environment       string           `json:"environment,omitempty"`
	APIKeyID          string           `json:"api_key_id,omitempty"`
	MerchantID        string           `json:"merchant_id,omitempty"`
	SandboxID         string           `json:"sandbox_id,omitempty"`
}

// UnmarshalJSON accepts the retired feedback setting so existing profiles keep
// loading. It is discarded and will disappear the next time the config is saved.
func (p *Profile) UnmarshalJSON(data []byte) error {
	type profileFields Profile
	var decoded struct {
		profileFields
		LegacyFeedback json.RawMessage `json:"agent_feedback_submission"`
	}
	if err := strictJSON(data, &decoded); err != nil {
		return err
	}
	*p = Profile(decoded.profileFields)
	return nil
}

type ProjectConfig struct {
	ContextID string `json:"context_id,omitempty"`
	Profile   string `json:"profile,omitempty"`
	Merchant  string `json:"merchant_id,omitempty"`
	Sandbox   string `json:"sandbox_id,omitempty"`
}

type ResolvedConfig struct {
	ProfileContextID  string            `json:"-"`
	ContextID         string            `json:"context,omitempty"`
	CredentialScope   string            `json:"-"`
	ProfileName       string            `json:"profile"`
	MerchantGuard     string            `json:"merchant_guard,omitempty"`
	SandboxGuard      string            `json:"sandbox_guard,omitempty"`
	Environment       string            `json:"environment,omitempty"`
	APIKeyID          string            `json:"api_key_id,omitempty"`
	MerchantID        string            `json:"merchant_id,omitempty"`
	SandboxID         string            `json:"sandbox_id,omitempty"`
	ProjectConfigPath string            `json:"project_config_path,omitempty"`
	GlobalConfigPath  string            `json:"global_config_path"`
	Sources           map[string]string `json:"sources"`
	Profiles          []map[string]any  `json:"profiles"`
}

func (a *App) configDirectory() (string, error) {
	if a.ConfigDir != "" {
		return a.ConfigDir, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "flint"), nil
}

func (a *App) configPath() (string, error) {
	dir, err := a.configDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func (a *App) loadConfig() (Config, error) {
	cfg := Config{DefaultProfile: "default", Profiles: map[string]Profile{}}
	path, err := a.configPath()
	if err != nil {
		return cfg, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := configJSON(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.DefaultProfile == "" {
		cfg.DefaultProfile = "default"
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	return cfg, nil
}

func configJSON(data []byte, target any) error {
	if strings.TrimSpace(string(data)) == "" {
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return errors.New("config must be a JSON object")
	}
	err := strictJSON(data, target)
	if err == nil {
		return nil
	}
	if _, project := target.(*ProjectConfig); project {
		for old, current := range map[string]string{"merchant": "merchant_id", "sandbox": "sandbox_id", "context": "context_id"} {
			if err.Error() == fmt.Sprintf("json: unknown field %q", old) {
				return fmt.Errorf("project field %q was renamed to %q", old, current)
			}
		}
		const prefix = "json: unknown field "
		if strings.HasPrefix(err.Error(), prefix) {
			field := strings.Trim(strings.TrimPrefix(err.Error(), prefix), "\"")
			candidates := []string{"merchant_id", "sandbox_id", "context_id", "profile"}
			sortStringsByDistance(candidates, field)
			if editDistance(field, candidates[0]) <= 3 {
				return fmt.Errorf("%w; did you mean %q?", err, candidates[0])
			}
		}
	}
	return err
}

func profileList(cfg Config) []map[string]any {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	profiles := make([]map[string]any, 0, len(names))
	for _, name := range names {
		p := cfg.Profiles[name]
		profiles = append(profiles, map[string]any{"name": name, "default": name == cfg.DefaultProfile, "environment": p.Environment, "merchant_id": p.MerchantID})
	}
	return profiles
}

func strictJSON(data []byte, target any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (a *App) saveConfig(cfg Config) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	return withLocalFileLock(a.commandContext(), path, func() error {
		return a.saveConfigUnlocked(cfg)
	})
}

func (a *App) saveConfigUnlocked(cfg Config) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), "config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func (a *App) updateConfig(update func(*Config) error) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	return withLocalFileLock(a.commandContext(), path, func() error {
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		if err := update(&cfg); err != nil {
			return err
		}
		return a.saveConfigUnlocked(cfg)
	})
}

func (a *App) withConfigLock(action func() error) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	return withLocalFileLock(a.commandContext(), path, action)
}

func (a *App) findProjectConfig() (ProjectConfig, string, error) {
	start := a.WorkingDir
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return ProjectConfig{}, "", err
		}
	}
	for {
		path := filepath.Join(start, ".flint", "config.json")
		b, err := os.ReadFile(path)
		if err == nil {
			var cfg ProjectConfig
			if err := configJSON(b, &cfg); err != nil {
				return cfg, path, fmt.Errorf("parse %s: %w", path, err)
			}
			return cfg, path, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return ProjectConfig{}, path, err
		}
		parent := filepath.Dir(start)
		if parent == start {
			return ProjectConfig{}, "", nil
		}
		start = parent
	}
}

func (a *App) resolveConfig(opts Options) (ResolvedConfig, Config, error) {
	project, projectPath, err := a.findProjectConfig()
	if err != nil {
		return ResolvedConfig{}, Config{}, err
	}
	return a.resolveProjectConfig(opts, project, projectPath)
}

func (a *App) resolveProjectConfig(opts Options, project ProjectConfig, projectPath string) (ResolvedConfig, Config, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return ResolvedConfig{}, cfg, err
	}
	profile := cfg.DefaultProfile
	sources := map[string]string{"profile": "global"}
	if project.Profile != "" {
		profile = project.Profile
		sources["profile"] = "project"
	}
	if env := strings.TrimSpace(os.Getenv("FLINT_PROFILE")); env != "" {
		profile = env
		sources["profile"] = "environment"
	}
	if opts.Profile != "" {
		profile = opts.Profile
		sources["profile"] = "flag"
	}
	if profile == "" {
		profile = "default"
	}
	p, ok := cfg.Profiles[profile]
	if !ok && profile != "default" && !opts.allowMissingProfile {
		source := configSource("profile", sources["profile"], projectPath)
		return ResolvedConfig{}, cfg, fmt.Errorf("unknown profile %q from %s; create it with flint login --profile %s, flint auth import --profile %s, or flint signup --profile %s", profile, source, profile, profile, profile)
	}
	merchant := p.MerchantGuard
	if merchant != "" {
		sources["merchant_guard"] = "profile"
	}
	if project.Merchant != "" {
		merchant = project.Merchant
		sources["merchant_guard"] = "project"
	}
	if env := strings.TrimSpace(os.Getenv("FLINT_MERCHANT")); env != "" {
		merchant = env
		sources["merchant_guard"] = "environment"
	}
	if opts.Merchant != "" {
		merchant = opts.Merchant
		sources["merchant_guard"] = "flag"
	}
	if merchant != "" && !strings.HasPrefix(merchant, "mer_") {
		return ResolvedConfig{}, cfg, fmt.Errorf("merchant guard must be a mer_ ID")
	}
	sandboxGuard := strings.TrimSpace(project.Sandbox)
	if sandboxGuard != "" {
		if !strings.HasPrefix(sandboxGuard, "test_") {
			return ResolvedConfig{}, cfg, fmt.Errorf("project sandbox guard must be a test_ ID")
		}
		sources["sandbox_guard"] = "project"
	}
	contextID := p.ContextID
	if contextID != "" {
		sources["context"] = "profile"
	}
	if project.ContextID != "" {
		contextID = project.ContextID
		sources["context"] = "project"
	}
	// Environment credentials replace the saved browser session, including its
	// configured context. Explicit --context remains an error for these tokens;
	// merchant and sandbox guards still apply independently.
	if environmentCredentialActive() {
		contextID = ""
		delete(sources, "context")
	}
	if opts.ContextID != "" {
		contextID = opts.ContextID
		sources["context"] = "flag"
	}
	path, _ := a.configPath()
	return ResolvedConfig{ProfileContextID: p.ContextID, ContextID: contextID, ProfileName: profile, MerchantGuard: merchant, SandboxGuard: sandboxGuard, Environment: p.Environment, APIKeyID: p.APIKeyID, MerchantID: p.MerchantID, SandboxID: p.SandboxID, ProjectConfigPath: projectPath, GlobalConfigPath: path, Sources: sources, Profiles: profileList(cfg)}, cfg, nil
}

func credentialFromEnvironment() string { return strings.TrimSpace(os.Getenv("FLINT_API_KEY")) }

// keyringTimeout bounds each OS keyring call. A locked keyring in an SSH,
// container, or agent session can wait forever for an unlock prompt nobody
// sees, and the keyring library offers no cancellation.
var keyringTimeout = 5 * time.Second

var errKeyringTimeout = errors.New("the OS keyring did not respond; it may be locked or unavailable in this session")

func withKeyringTimeout[T any](call func() (T, error)) (T, error) {
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := call()
		done <- result{value, err}
	}()
	timer := time.NewTimer(keyringTimeout)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.value, r.err
	case <-timer.C:
		var zero T
		return zero, errKeyringTimeout
	}
}

func loadKeychainCredential(profile string) (string, error) {
	value, err := withKeyringTimeout(func() (string, error) { return keyring.Get(keyringService, profile) })
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return strings.TrimSpace(value), err
}

func storeKeychainCredential(profile, key string) error {
	_, err := withKeyringTimeout(func() (struct{}, error) { return struct{}{}, keyring.Set(keyringService, profile, key) })
	return err
}
func deleteKeychainCredential(profile string) error {
	_, err := withKeyringTimeout(func() (struct{}, error) { return struct{}{}, keyring.Delete(keyringService, profile) })
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// credentialStoreError explains a keyring failure with its cause and the
// environment-variable alternative that works without a keyring.
func credentialStoreError(code, message string, err error) *CLIError {
	if err != nil && !errors.Is(err, context.Canceled) {
		message = strings.TrimSuffix(message, ".") + ": " + terminalSafe(err.Error()) + "."
	}
	e := configError(code, message+" To save credentials without an OS keyring, use --credential-store file, or set FLINT_API_KEY.", err)
	e.Details = map[string]any{"remediation": map[string]any{"next_actions": []any{
		map[string]any{"reason_message": "Set FLINT_API_KEY to a Flint API key; it overrides saved credentials and needs no keyring."},
		map[string]any{"command": "flint login --credential-store file"},
	}}}
	return e
}

func (a *App) resolveCredential(profile string) (string, string, error) {
	if key := credentialFromEnvironment(); key != "" {
		return key, "environment", nil
	}
	if workloadIdentityID() != "" {
		return workloadCredential, "workload_identity", nil
	}
	key, err := a.loadSavedCredential(profile)
	if err != nil {
		return "", "", err
	}
	if key != "" {
		cfg, _ := a.loadConfig()
		if a.selectedCredentialStore(profile, cfg.Profiles[profile]) == "file" {
			return key, "file", nil
		}
		return key, "keychain", nil
	}
	return "", "", nil
}

func invalidCredentialError(key string, err error) *CLIError {
	message := err.Error()
	if strings.HasPrefix(message, "FLINT_BASE_URL") {
		return configError("INVALID_BASE_URL", message, err)
	}
	if key != "" && key == credentialFromEnvironment() {
		message = "The key from FLINT_API_KEY is invalid: " + message
	}
	return configError("CREDENTIAL_FORMAT_INVALID", message, err)
}

func credentialEnvironment(key string) (string, error) {
	switch {
	case strings.HasPrefix(key, "flint_test_"):
		return "sandbox", nil
	case strings.HasPrefix(key, "flint_live_"):
		return "live", nil
	default:
		return "", errors.New("credential must start with flint_test_ or flint_live_")
	}
}
