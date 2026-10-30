package cli

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
)

func configSource(key, source, projectPath string) string {
	switch source {
	case "flag":
		if key == "merchant_guard" {
			return "--merchant"
		}
		return "--" + key
	case "environment":
		if key == "merchant_guard" {
			return "FLINT_MERCHANT"
		}
		return "FLINT_" + strings.ToUpper(key)
	case "project":
		return projectPath
	case "profile":
		return "profile"
	default:
		return "global config"
	}
}

func validateResolvedCredentialIntent(auth AuthContext, opts Options, resolved ResolvedConfig, affinity EnvironmentAffinity) *CLIError {
	e := validateCredentialIntentForCommand(auth, opts, resolved.MerchantGuard, resolved.SandboxGuard, affinity)
	if e == nil {
		return nil
	}
	key := ""
	if e.Code == "MERCHANT_GUARD_MISMATCH" {
		key = "merchant_guard"
	}
	if e.Code == "SANDBOX_GUARD_MISMATCH" {
		key = "sandbox_guard"
	}
	if key != "" {
		e.Message += " Guard source: " + configSource(key, resolved.Sources[key], resolved.ProjectConfigPath) + "."
	}
	return e
}

func configUnknownFieldWarnings(resolved ResolvedConfig) []map[string]any {
	var warnings []map[string]any
	for _, item := range []struct {
		path   string
		known  []string
		global bool
	}{
		{resolved.GlobalConfigPath, []string{"default_profile", "profiles"}, true},
		{resolved.ProjectConfigPath, []string{"context", "profile", "merchant", "sandbox"}, false},
	} {
		raw, err := os.ReadFile(item.path)
		if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			continue
		}
		known := map[string]bool{}
		for _, key := range item.known {
			known[key] = true
		}
		var unknown []string
		for key := range fields {
			if !known[key] {
				unknown = append(unknown, key)
			}
		}
		if item.global {
			var profiles map[string]map[string]json.RawMessage
			_ = json.Unmarshal(fields["profiles"], &profiles)
			allowed := map[string]bool{"credential_store": true, "credential_present": true, "pending_signup": true, "context": true, "merchant_guard": true, "environment": true, "api_key_id": true, "merchant_id": true, "sandbox_id": true, "agent_feedback_submission": true}
			for profile, fields := range profiles {
				for key := range fields {
					if !allowed[key] {
						unknown = append(unknown, "profiles."+profile+"."+key)
					}
				}
			}
		}
		sort.Strings(unknown)
		for _, key := range unknown {
			warnings = append(warnings, map[string]any{"name": "unknown_config_field", "status": "warning", "path": item.path, "field": key, "message": "Unknown field is ignored."})
		}
	}
	return warnings
}
