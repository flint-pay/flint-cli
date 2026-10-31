package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

func calendarTime(value string) bool {
	if value == "today" || value == "yesterday" || value == "this_week" {
		return true
	}
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}

func resolveTime(value string, now time.Time, locations ...*time.Location) (string, error) {
	location := time.UTC
	if len(locations) > 0 && locations[0] != nil {
		location = locations[0]
	}
	if value == "now" {
		return now.UTC().Format(time.RFC3339), nil
	}
	if strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		past, err := resolveTime(value[1:], now, location)
		if err != nil {
			return "", err
		}
		instant, _ := time.Parse(time.RFC3339, past)
		if value[0] == '+' {
			instant = now.Add(now.Sub(instant))
		}
		return instant.UTC().Format(time.RFC3339), nil
	}
	if calendarTime(value) {
		local := now.In(location)
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		switch value {
		case "today":
		case "yesterday":
			midnight = midnight.AddDate(0, 0, -1)
		case "this_week":
			midnight = midnight.AddDate(0, 0, -(int(midnight.Weekday())+6)%7)
		default:
			midnight, _ = time.ParseInLocation(time.DateOnly, value, location)
		}
		return midnight.UTC().Format(time.RFC3339), nil
	}
	if d, err := time.ParseDuration(value); err == nil && d > 0 {
		return now.Add(-d).UTC().Format(time.RFC3339), nil
	}
	if len(value) > 1 && (strings.HasSuffix(value, "d") || strings.HasSuffix(value, "w")) {
		n, err := strconv.ParseInt(value[:len(value)-1], 10, 64)
		hours := int64(24)
		if strings.HasSuffix(value, "w") {
			hours *= 7
		}
		if err == nil && n > 0 && n <= int64((1<<63-1)/time.Hour)/hours {
			return now.Add(-time.Duration(n*hours) * time.Hour).UTC().Format(time.RFC3339), nil
		}
	}
	instant, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return "", fmt.Errorf("time must be RFC3339, YYYY-MM-DD, today, yesterday, this_week, or a duration such as 2h, 7d, or 2w")
	}
	return instant.UTC().Format(time.RFC3339), nil
}

type reportingTimezoneCache struct {
	StoredAt time.Time `json:"stored_at"`
	Timezone string    `json:"timezone"`
}

// Store only the public timezone and a timestamp, alongside the auth-context
// cache. Hash the credential and selected context so profiles cannot cross over.
func (a *App) reportingTimezone(ctx context.Context, opts Options, resolved ResolvedConfig, key, baseURL string) (*time.Location, string) {
	fallback := "Reporting timezone unavailable; using UTC (requires merchants.profile.read, or a cached timezone for offline dry runs)."
	cachePath := ""
	if dir, err := a.configDirectory(); err == nil && key != "" {
		digest := sha256.Sum256([]byte(baseURL + "\x00" + key + "\x00" + resolved.ContextID + "\x00" + resolved.MerchantID + "\x00" + resolved.SandboxID))
		cachePath = filepath.Join(dir, "reporting-timezone-"+hex.EncodeToString(digest[:])+".json")
		var entry reportingTimezoneCache
		if raw, err := os.ReadFile(cachePath); err == nil && json.Unmarshal(raw, &entry) == nil {
			age := a.Now().Sub(entry.StoredAt)
			if age >= 0 && age < time.Hour {
				if location, err := time.LoadLocation(entry.Timezone); err == nil {
					return location, ""
				}
			}
		}
	}
	if opts.DryRunClient() || opts.Preview || key == "" {
		return time.UTC, fallback
	}
	ctx = context.WithValue(ctx, responseContractKey{}, (*Command)(nil))
	ctx, cancel := context.WithTimeout(ctx, min(opts.Timeout, 5*time.Second))
	defer cancel()
	response, err := a.doRequest(ctx, baseURL, key, "GET", "/v1/merchant", nil, "", opts.Debug)
	if err != nil {
		return time.UTC, fallback
	}
	zone, ok := lookupPath(response.Value, "data.reporting_timezone")
	if !ok {
		return time.UTC, fallback
	}
	name, ok := zone.(string)
	if !ok || name == "" {
		return time.UTC, fallback
	}
	location, loadErr := time.LoadLocation(name)
	if loadErr != nil {
		return time.UTC, fallback
	}
	if cachePath != "" {
		_ = writePrivateJSON(cachePath, reportingTimezoneCache{StoredAt: a.Now(), Timezone: name})
	}
	return location, ""
}
