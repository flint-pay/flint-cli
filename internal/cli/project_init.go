package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// The caller has validated the existing project's guards against auth-context.
func (a *App) writeProjectConfig(path string, project ProjectConfig, resolved ResolvedConfig, force bool) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	written := false
	err := withLocalFileLock(a.commandContext(), path, func() error {
		if !force {
			raw, err := os.ReadFile(path)
			if err == nil {
				var existing ProjectConfig
				if err := configJSON(raw, &existing); err != nil {
					return err
				}
				// Refuse a concurrent change after the credential guard check.
				if (existing.ContextID != "" && existing.ContextID != project.ContextID) || (existing.Profile != "" && existing.Profile != resolved.ProfileName) || (existing.Merchant != "" && existing.Merchant != project.Merchant) || (existing.Sandbox != "" && existing.Sandbox != project.Sandbox) {
					return errors.New("project config changed during init; run flint init again")
				}
				return nil
			}
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		raw, err := json.MarshalIndent(project, "", "  ")
		if err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), "config-*.tmp")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(append(raw, '\n')); err != nil {
			tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		if err := os.Rename(tmp.Name(), path); err != nil {
			return err
		}
		written = true
		return nil
	})
	return written, err
}
