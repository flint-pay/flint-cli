package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type loginDevice struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

type pendingLogin struct {
	CredentialStore  string      `json:"credential_store,omitempty"`
	Device           loginDevice `json:"device"`
	ExpiresAt        time.Time   `json:"expires_at"`
	BaseURL          string      `json:"base_url"`
	RequestedScopes  []string    `json:"requested_scopes"`
	AdditionalScopes []string    `json:"additional_scopes,omitempty"`
	Profile          string      `json:"profile"`
	Live             bool        `json:"live"`
	MerchantGuard    string      `json:"merchant_guard,omitempty"`
	SandboxGuard     string      `json:"sandbox_guard,omitempty"`
	ContextID        string      `json:"context,omitempty"`
}

func (pending pendingLogin) nextCommand() string {
	command := "flint login"
	if pending.Live {
		command += " --live"
	}
	command += " --profile " + quoteRemediationArgument(pending.Profile)
	if pending.CredentialStore != "" {
		command += " --credential-store " + pending.CredentialStore
	}
	if pending.MerchantGuard != "" {
		command += " --merchant " + quoteRemediationArgument(pending.MerchantGuard)
	}
	for _, scope := range pending.AdditionalScopes {
		command += " --scope " + quoteRemediationArgument(scope)
	}
	return command
}

func (a *App) pendingLoginPath(profile string) (string, error) {
	dir, err := a.configDirectory()
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(profile))
	return filepath.Join(dir, "pending-login-"+hex.EncodeToString(hash[:])+".json"), nil
}

func (a *App) loadPendingLogin(profile, baseURL string) (*pendingLogin, error) {
	path, err := a.pendingLoginPath(profile)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pending pendingLogin
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, errors.New("saved pending login is invalid; remove " + path + " and retry")
	}
	if pending.CredentialStore != "" && pending.CredentialStore != "keyring" && pending.CredentialStore != "file" {
		return nil, errors.New("saved pending login credential_store must be keyring or file")
	}
	if !a.Now().Before(pending.ExpiresAt) {
		return nil, os.Remove(path)
	}
	if pending.Profile != profile || pending.BaseURL != baseURL {
		return nil, errors.New("pending login belongs to a different Flint server; use its original FLINT_BASE_URL or flint login --new-session")
	}
	return &pending, nil
}
