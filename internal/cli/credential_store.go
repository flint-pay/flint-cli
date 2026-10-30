package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func profileWithoutCredential() Profile {
	present := false
	return Profile{CredentialPresent: &present}
}

func missingCredentialMessage(profile string) string {
	return fmt.Sprintf("No Flint credential is configured for profile '%s'. Run flint login, run flint signup to create an account, or set FLINT_API_KEY.", profile)
}

func (a *App) selectedCredentialStore(_ string, p Profile) string {
	if a.credentialStore != "" {
		return a.credentialStore
	}
	if p.CredentialStore != "" {
		return p.CredentialStore
	}
	return "keyring"
}

func (a *App) profileWithCredential(profile string, p Profile, present bool) Profile {
	p.CredentialStore = a.selectedCredentialStore(profile, p)
	p.CredentialPresent = &present
	return p
}

func (a *App) credentialFilePath() (string, error) {
	dir, err := a.configDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials", "store.json"), nil
}

// The private store has its own lock. Lifecycle transactions always acquire the
// config lock first, then this lock, so refresh/import/logout serialize together.
func (a *App) fileCredential(profile string, update *string) (string, error) {
	path, err := a.credentialFilePath()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("credential directory must be a real directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	var value string
	err = withLocalFileLock(a.commandContext(), path, func() error {
		credentials := map[string]string{}
		info, err := os.Lstat(path)
		if err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("credential store must be a regular file")
			}
			if err := os.Chmod(path, 0600); err != nil {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &credentials); err != nil {
				return errors.New("could not parse the credential file")
			}
			if credentials == nil {
				credentials = map[string]string{}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		value = credentials[profile]
		if update == nil {
			return nil
		}
		if *update == "" {
			delete(credentials, profile)
		} else {
			credentials[profile] = *update
		}
		return writePrivateJSON(path, credentials)
	})
	return value, err
}

func (a *App) rawSavedCredential(profile string, p Profile) (string, error) {
	switch a.selectedCredentialStore(profile, p) {
	case "keyring":
		return a.LoadCredential(profile)
	case "file":
		return a.fileCredential(profile, nil)
	default:
		return "", errors.New("configured credential_store must be keyring or file")
	}
}

func (a *App) loadSavedCredential(profile string) (string, error) {
	var value string
	err := a.withConfigLock(func() error {
		var err error
		value, err = a.loadSavedCredentialUnlocked(profile)
		return err
	})
	return value, err
}

// Call only while holding the config lock. A missing profile is genuinely new;
// an unmarked existing profile receives one bounded legacy keyring lookup.
func (a *App) loadSavedCredentialUnlocked(profile string) (string, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return "", err
	}
	p, exists := cfg.Profiles[profile]
	if p.CredentialStore != "" && p.CredentialStore != "keyring" && p.CredentialStore != "file" {
		return "", errors.New("configured credential_store must be keyring or file")
	}
	if !exists || (p.CredentialPresent != nil && !*p.CredentialPresent && (a.credentialStore == "" || p.CredentialStore == "" || a.credentialStore == p.CredentialStore)) {
		return "", nil
	}
	value, err := a.rawSavedCredential(profile, p)
	if err != nil {
		return "", err
	}
	selected := a.selectedCredentialStore(profile, p)

	if p.CredentialPresent == nil && selected == "keyring" {
		p = a.profileWithCredential(profile, p, value != "")
		cfg.Profiles[profile] = p
		if err := a.saveConfigUnlocked(cfg); err != nil {
			return "", err
		}
	}
	return value, nil
}

// These raw writes do not change profile metadata. Their caller commits the
// presence/backend marker with the rest of the profile, or restores on failure.
func (a *App) storeSavedCredential(profile, value string) error {
	var p Profile
	if a.credentialStore == "" {
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		p = cfg.Profiles[profile]
	}
	switch a.selectedCredentialStore(profile, p) {
	case "keyring":
		return a.StoreCredential(profile, value)
	case "file":
		_, err := a.fileCredential(profile, &value)
		return err
	default:
		return errors.New("configured credential_store must be keyring or file")
	}
}

func (a *App) deleteSavedCredential(profile string) error {
	var p Profile
	if a.credentialStore == "" {
		cfg, err := a.loadConfig()
		if err != nil {
			return err
		}
		p = cfg.Profiles[profile]
	}
	switch a.selectedCredentialStore(profile, p) {
	case "keyring":
		return a.DeleteCredential(profile)
	case "file":
		empty := ""
		_, err := a.fileCredential(profile, &empty)
		return err
	default:
		return errors.New("configured credential_store must be keyring or file")
	}
}

func (a *App) credentialSavedNotice(profile string) {
	cfg, err := a.loadConfig()
	if err != nil || a.selectedCredentialStore(profile, cfg.Profiles[profile]) != "file" {
		return
	}
	if path, err := a.credentialFilePath(); err == nil {
		fmt.Fprintf(a.Stderr, "Credential saved to %s with owner-only permissions. This file is not encrypted by the OS keyring.\n", terminalSafe(path))
	}
}
