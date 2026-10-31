package cli

import (
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
