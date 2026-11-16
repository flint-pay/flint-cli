package cli

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

func validSignupTimezone(zone string) bool {
	if zone == "" || zone == "Local" || strings.HasPrefix(zone, "/") {
		return false
	}
	_, err := time.LoadLocation(zone)
	return err == nil
}

// Prefer TZ when explicitly configured. Never guess an IANA zone from a UTC
// offset or abbreviation, since those do not identify daylight-saving rules.
func localSignupTimezone() string {
	if zone, set := os.LookupEnv("TZ"); set {
		if zone == "" {
			return "UTC"
		}
		zone = strings.TrimPrefix(zone, ":")
		if validSignupTimezone(zone) {
			return zone
		}
		return ""
	}
	if zone := time.Local.String(); validSignupTimezone(zone) {
		return zone
	}
	if path, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(filepath.ToSlash(path), "/zoneinfo/"); ok && validSignupTimezone(zone) {
			return zone
		}
	}
	if raw, err := os.ReadFile("/etc/timezone"); err == nil {
		if zone := strings.TrimSpace(string(raw)); validSignupTimezone(zone) {
			return zone
		}
	}
	return ""
}

func signupTimezone(opts Options) (string, *CLIError) {
	if values, supplied := opts.Raw["timezone"]; supplied {
		zone := ""
		if len(values) > 0 {
			zone = values[len(values)-1]
		}
		if !validSignupTimezone(zone) {
			return "", usageError("INVALID_TIMEZONE", "--timezone must be an IANA time zone, such as America/Chicago.", "timezone")
		}
		return zone, nil
	}
	return localSignupTimezone(), nil
}
