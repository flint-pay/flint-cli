package cli

import "context"

// Public diagnostics remain useful when no credential can be loaded. Keep the
// authentication failure's exit code while finishing independent checks.
func (a *App) finishDoctorChecks(checks []map[string]any, opts Options, baseURL string) []map[string]any {
	hasVersion, hasCLI, hasConnectivity, connectivityFailed := false, false, false, false
	for _, check := range checks {
		switch check["name"] {
		case "api_version":
			hasVersion = true
		case "cli_version":
			hasCLI = true
		case "connectivity":
			hasConnectivity = true
			connectivityFailed = check["status"] == "fail"
		}
	}
	if !hasVersion && !connectivityFailed && a.commandContext().Err() == nil {
		var err error
		if baseURL == "" {
			baseURL, err = a.baseURLForCredential("flint_test_")
		}
		var observed string
		var discoveryError *CLIError
		if err != nil {
			discoveryError = configError("INVALID_BASE_URL", err.Error(), err)
		} else {
			ctx, cancel := context.WithTimeout(a.commandContext(), opts.Timeout)
			observed, discoveryError = a.fetchCurrentAPIVersion(ctx, baseURL, opts.Debug)
			cancel()
		}
		if !hasConnectivity {
			connectivity := map[string]any{"name": "connectivity", "status": "pass"}
			if discoveryError != nil && discoveryError.ExitCode == ExitNetwork {
				connectivity["status"] = "fail"
				connectivity["fix"] = discoveryError.Message
			}
			checks = append(checks, connectivity)
		}
		version := map[string]any{"name": "api_version", "expected": a.Info.APIVersion, "observed": observed, "status": "pass"}
		if discoveryError != nil {
			version["status"] = "warning"
			version["fix"] = discoveryError.Message
			if discoveryError.RequestID != "" {
				version["request_id"] = discoveryError.RequestID
			}
		} else if observed != a.Info.APIVersion {
			version["status"] = "info"
			version["note"] = "The server's current API version is " + observed + ". This CLI requests " + a.Info.APIVersion + "."
			version["changelog_url"] = "https://developers.withflintpay.com/changelog"
		}
		checks = append(checks, version)
	}
	if !hasCLI {
		checks = append(checks, map[string]any{"name": "cli_version", "status": "pass", "installed": a.Info.Version, "note": "Run flint upgrade to check for a newer stable CLI release."})
	}
	return checks
}
