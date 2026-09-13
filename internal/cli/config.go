package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

const keyringService = "flint-cli"

type Config struct {
	DefaultProfile string             `json:"default_profile"`
	Profiles       map[string]Profile `json:"profiles"`
}

type Profile struct {
	MerchantGuard           string `json:"merchant_guard,omitempty"`
	Environment             string `json:"environment,omitempty"`
	APIKeyID                string `json:"api_key_id,omitempty"`
	MerchantID              string `json:"merchant_id,omitempty"`
	SandboxID               string `json:"sandbox_id,omitempty"`
	AgentFeedbackSubmission string `json:"agent_feedback_submission,omitempty"`
}

type ProjectConfig struct {
	Profile  string `json:"profile,omitempty"`
	Merchant string `json:"merchant,omitempty"`
	Sandbox  string `json:"sandbox,omitempty"`
}

type ResolvedConfig struct {
	CredentialScope         string            `json:"-"`
	ProfileName             string            `json:"profile"`
	MerchantGuard           string            `json:"merchant_guard,omitempty"`
	SandboxGuard            string            `json:"sandbox_guard,omitempty"`
	Environment             string            `json:"environment,omitempty"`
	APIKeyID                string            `json:"api_key_id,omitempty"`
	MerchantID              string            `json:"merchant_id,omitempty"`
	SandboxID               string            `json:"sandbox_id,omitempty"`
	AgentFeedbackSubmission string            `json:"agent_feedback_submission,omitempty"`
	ProjectConfigPath       string            `json:"project_config_path,omitempty"`
	GlobalConfigPath        string            `json:"global_config_path"`
	Sources                 map[string]string `json:"sources"`
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
	if err := strictJSON(b, &cfg); err != nil {
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
	return withLocalFileLock(path, func() error {
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
	return withLocalFileLock(path, func() error {
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
	return withLocalFileLock(path, action)
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
			if err := strictJSON(b, &cfg); err != nil {
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
	cfg, err := a.loadConfig()
	if err != nil {
		return ResolvedConfig{}, cfg, err
	}
	project, projectPath, err := a.findProjectConfig()
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
	if !ok && profile != "default" {
		return ResolvedConfig{}, cfg, fmt.Errorf("unknown profile %q", profile)
	}
	if p.AgentFeedbackSubmission != "" && p.AgentFeedbackSubmission != "enabled" && p.AgentFeedbackSubmission != "disabled" {
		return ResolvedConfig{}, cfg, fmt.Errorf("profile %q agent_feedback_submission must be enabled or disabled", profile)
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
	path, _ := a.configPath()
	return ResolvedConfig{ProfileName: profile, MerchantGuard: merchant, SandboxGuard: sandboxGuard, Environment: p.Environment, APIKeyID: p.APIKeyID, MerchantID: p.MerchantID, SandboxID: p.SandboxID, AgentFeedbackSubmission: p.AgentFeedbackSubmission, ProjectConfigPath: projectPath, GlobalConfigPath: path, Sources: sources}, cfg, nil
}

func credentialFromEnvironment() string { return strings.TrimSpace(os.Getenv("FLINT_API_KEY")) }

func loadKeychainCredential(profile string) (string, error) {
	value, err := keyring.Get(keyringService, profile)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	return strings.TrimSpace(value), err
}

func storeKeychainCredential(profile, key string) error {
	return keyring.Set(keyringService, profile, key)
}
func deleteKeychainCredential(profile string) error {
	err := keyring.Delete(keyringService, profile)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func (a *App) resolveCredential(profile string) (string, string, error) {
	if key := credentialFromEnvironment(); key != "" {
		return key, "environment", nil
	}
	key, err := a.LoadCredential(profile)
	if err != nil {
		return "", "", err
	}
	if key != "" {
		return key, "keychain", nil
	}
	return "", "", nil
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
