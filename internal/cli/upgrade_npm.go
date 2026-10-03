package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *App) upgradeWithNPM(ctx context.Context, opts Options, version, root string) (string, *CLIError) {
	wrapper := filepath.Join(root, "@flintpay", "cli")
	writeFailure := func(err error) *CLIError {
		return upgradeFailure("UPGRADE_WRITE_FAILED", "Could not replace the npm Flint CLI package at "+wrapper+". Check that its directory is writable.", err)
	}
	info, err := os.Lstat(wrapper)
	if err != nil {
		return "", writeFailure(err)
	}
	if !info.IsDir() {
		return "", writeFailure(errors.New("the npm package path is not a directory"))
	}
	// Keep staging and backup beside the package so promotion uses a rename on
	// the same filesystem. The existing npm bin link points at this stable path.
	temporary, err := os.MkdirTemp(filepath.Dir(wrapper), ".flint-npm-upgrade-*")
	if err != nil {
		return "", writeFailure(err)
	}
	keepBackup := false
	defer func() {
		if !keepBackup {
			os.RemoveAll(temporary)
		}
	}()
	// --prefix changes npm's default globalconfig path. Preserve the owning
	// npm installation's config, including scoped registries and credentials.
	configOutput, err := a.runCommand(ctx, "npm", "config", "get", "globalconfig")
	if ctx.Err() != nil {
		return "", networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
	}
	globalConfig := strings.TrimSpace(string(configOutput))
	if err != nil || !filepath.IsAbs(globalConfig) || strings.ContainsAny(globalConfig, "\r\n") {
		return "", upgradeFailure("PACKAGE_MANAGER_FAILED", "Could not locate the npm global configuration. The existing installation was preserved.", err)
	}
	prefix := filepath.Join(temporary, "prefix")
	staged := filepath.Join(prefix, "lib", "node_modules", "@flintpay", "cli")
	a.withUpgradeProgress(ctx, opts, "Installing Flint CLI "+version+" with npm", func() {
		_, err = a.runCommand(ctx, "npm", "install", "-g", "--prefix", prefix, "@flintpay/cli@"+version, "--include=optional", "--no-audit", "--no-fund", "--globalconfig", globalConfig)
	})
	if ctx.Err() != nil {
		return "", networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
	}
	if err != nil {
		return "", upgradeFailure("PACKAGE_MANAGER_FAILED", "The npm upgrade failed. The existing installation was preserved.", err)
	}
	// Global npm installs nest dependencies inside the wrapper. Check this
	// binary explicitly: Node resolution must not fall back to an older package
	// outside staging when npm silently skips an unavailable optional dependency.
	platform := filepath.Join(staged, "node_modules", "@flintpay", "cli-"+a.runtimeGOOS+"-"+a.runtimeGOARCH, "bin", "flint")
	resolved, err := filepath.EvalSymlinks(platform)
	stagedResolved, stagedErr := filepath.EvalSymlinks(staged)
	if err != nil || stagedErr != nil || !pathWithin(stagedResolved, resolved) {
		return "", npmVerificationFailure(version, staged, nil, err)
	}
	if info, err := os.Stat(resolved); err != nil || !info.Mode().IsRegular() {
		return "", npmVerificationFailure(version, staged, nil, err)
	}
	var verificationErr *CLIError
	a.withUpgradeProgress(ctx, opts, "Verifying downloaded Flint CLI "+version, func() {
		verificationErr = a.verifyNPMWrapper(ctx, staged, version)
	})
	if verificationErr != nil {
		return "", verificationErr
	}
	installedVersion := version
	err = withLocalFileLock(ctx, filepath.Join(filepath.Dir(wrapper), ".flint-upgrade"), func() error {
		current, currentErr := a.newerNPMInstallation(ctx, wrapper, version)
		if currentErr != nil {
			return currentErr
		}
		if current != "" {
			installedVersion = current
			return nil
		}
		backup := filepath.Join(temporary, "previous")
		if err := os.Rename(wrapper, backup); err != nil {
			return writeFailure(err)
		}
		rollback := func(failure *CLIError) *CLIError {
			if err := os.RemoveAll(wrapper); err != nil {
				keepBackup = true
				return npmRollbackFailure(failure, backup, err)
			}
			if err := os.Rename(backup, wrapper); err != nil {
				keepBackup = true
				return npmRollbackFailure(failure, backup, err)
			}
			return failure
		}
		if err := os.Rename(staged, wrapper); err != nil {
			return rollback(writeFailure(err))
		}
		a.withUpgradeProgress(ctx, opts, "Verifying installed Flint CLI "+version, func() {
			verificationErr = a.verifyNPMWrapper(ctx, wrapper, version)
		})
		if verificationErr != nil {
			return rollback(verificationErr)
		}
		return nil
	})
	if err != nil {
		var failure *CLIError
		if errors.As(err, &failure) {
			return "", failure
		}
		if ctx.Err() != nil {
			return "", networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
		}
		return "", writeFailure(err)
	}
	return installedVersion, nil
}

func (a *App) verifyNPMWrapper(ctx context.Context, wrapper, version string) *CLIError {
	output, err := a.runCommand(ctx, "node", filepath.Join(wrapper, "bin", "flint.js"), "version", "--field", "data.cli_version", "--color", "never")
	if ctx.Err() != nil {
		return networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
	}
	if err == nil && strings.TrimSpace(string(output)) == version {
		return nil
	}
	return npmVerificationFailure(version, wrapper, output, err)
}

func (a *App) newerNPMInstallation(ctx context.Context, wrapper, version string) (string, *CLIError) {
	// A slower download must not overwrite a newer installation promoted by
	// another updater. Read metadata under the transaction lock, then confirm
	// that the newer package actually runs before keeping it.
	raw, err := os.ReadFile(filepath.Join(wrapper, "package.json"))
	var manifest struct {
		Version string `json:"version"`
	}
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		return "", nil
	}
	current, currentOK := parseReleaseVersion(manifest.Version)
	expected, expectedOK := parseReleaseVersion(version)
	if !currentOK || !expectedOK || compareReleaseVersions(current, expected) < 0 {
		return "", nil
	}
	output, err := a.runCommand(ctx, "node", filepath.Join(wrapper, "bin", "flint.js"), "version", "--field", "data.cli_version", "--color", "never")
	if ctx.Err() != nil {
		return "", networkError("REQUEST_CANCELED", "The CLI upgrade was canceled or timed out.", ctx.Err())
	}
	actual, actualOK := parseReleaseVersion(string(output))
	if err == nil && actualOK && compareReleaseVersions(actual, expected) >= 0 {
		return actual.raw, nil
	}
	return "", nil
}

func npmVerificationFailure(version, wrapper string, output []byte, err error) *CLIError {
	failure := upgradeFailure("UPGRADE_VERIFICATION_FAILED", "The npm package could not run Flint CLI "+version+". The existing installation was preserved. Retry flint upgrade after the npm platform package is available.", err)
	details := map[string]any{"install_method": "npm", "expected_version": version, "executable": filepath.Join(wrapper, "bin", "flint.js"), "retry_command": "flint upgrade"}
	if installed, valid := parseReleaseVersion(string(output)); valid {
		details["installed_version"] = installed.raw
	}
	failure.Details = details
	return failure
}

func npmRollbackFailure(failure *CLIError, backup string, err error) *CLIError {
	result := upgradeFailure("UPGRADE_ROLLBACK_FAILED", fmt.Sprintf("The npm upgrade failed and could not restore the previous installation. The previous package is preserved at %s.", backup), err)
	result.Details = map[string]any{"backup_path": backup, "upgrade_error": failure.Code}
	return result
}
