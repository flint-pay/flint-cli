package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpgradeCheckReleaseDiscovery(t *testing.T) {
	const tagPath = "/flint-pay/flint-cli/releases/tag/cli/v"
	const token = "upgrade-test-token"
	for _, tc := range []struct {
		name         string
		status       int
		location     string
		networkError bool
		fallback     bool
	}{
		{name: "redirect", status: 302, location: tagPath + "1.2.0"},
		{name: "permanent redirect", status: 308, location: tagPath + "1.2.0"},
		{name: "no redirect", status: 200, location: tagPath + "9.0.0", fallback: true},
		{name: "missing location", status: 302, fallback: true},
		{name: "invalid version", status: 302, location: tagPath + "01.2.0", fallback: true},
		{name: "prerelease", status: 302, location: tagPath + "9.0.0-beta.1", fallback: true},
		{name: "build metadata", status: 302, location: tagPath + "9.0.0+build", fallback: true},
		{name: "other repository", status: 302, location: "/other/repo/releases/tag/cli/v9.0.0", fallback: true},
		{name: "other host", status: 302, location: "https://example.com" + tagPath + "9.0.0", fallback: true},
		{name: "query", status: 302, location: tagPath + "9.0.0?x=1", fallback: true},
		{name: "fragment", status: 302, location: tagPath + "9.0.0#fragment", fallback: true},
		{name: "trailing slash", status: 302, location: tagPath + "9.0.0/", fallback: true},
		{name: "network error", networkError: true, fallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", token)
			apiRequests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/latest":
					if r.Header.Get("Authorization") != "" {
						t.Error("redirect lookup received a credential")
					}
					// Use an absolute URL, as GitHub does in production.
					location := tc.location
					if strings.HasPrefix(location, "/") {
						location = "http://" + r.Host + location
					}
					if location != "" {
						w.Header().Set("Location", location)
					}
					w.WriteHeader(tc.status)
				case "/api":
					apiRequests++
					if r.Header.Get("Authorization") != "Bearer "+token {
						t.Error("API fallback did not receive GITHUB_TOKEN")
					}
					fmt.Fprint(w, `[{"tag_name":"cli/v9.0.0","draft":true},{"tag_name":"cli/v8.0.0","prerelease":true},{"tag_name":"cli/v1.2.0"}]`)
				default:
					t.Error("lookup followed a redirect or downloaded an asset")
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, "")
			app.Info.Version = "1.0.0"
			app.upgradeReleaseLatestURL = server.URL + "/latest"
			app.upgradeReleaseAPIURL = server.URL + "/api"
			if tc.networkError {
				app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/latest" {
						return nil, errors.New("redirect connection failed")
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
			}
			app.executablePath = func() (string, error) { t.Fatal("--check looked up an executable"); return "", nil }
			if exit := app.Run([]string{"upgrade", "--check", "--output", "json", "--debug"}); exit != ExitOK {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			var got struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Data["latest_version"] != "1.2.0" || got.Data["current_version"] != "1.0.0" || got.Data["update_available"] != true || got.Data["status"] != "update_available" {
				t.Fatalf("unexpected check result: %s", stdout)
			}
			if (apiRequests == 1) != tc.fallback || apiRequests > 1 {
				t.Fatalf("API requests=%d fallback=%t", apiRequests, tc.fallback)
			}
			if strings.Contains(stdout.String()+stderr.String(), token) {
				t.Fatal("output exposed GITHUB_TOKEN")
			}
		})
	}
}

func TestUpgradeCheckGitHubRateLimitOutput(t *testing.T) {
	const resetAt = "2030-01-02T03:04:05Z"
	reset, _ := time.Parse(time.RFC3339, resetAt)
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
		reset  string
	}{
		{"forbidden with reset", 403, http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}, resetAt},
		{"too many requests with reset", 429, http.Header{"X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}, resetAt},
		{"retry after date", 403, http.Header{"Retry-After": {reset.Format(http.TimeFormat)}}, resetAt},
		{"unknown reset", 429, http.Header{"X-Ratelimit-Remaining": {"0"}}, ""},
		{"other forbidden", 403, http.Header{}, ""},
	} {
		for _, output := range []string{"json", "human"} {
			t.Run(tc.name+"/"+output, func(t *testing.T) {
				const token = "upgrade-test-token"
				t.Setenv("GITHUB_TOKEN", token)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer "+token {
						t.Error("API fallback did not receive GITHUB_TOKEN")
					}
					for key, values := range tc.header {
						w.Header()[key] = values
					}
					w.WriteHeader(tc.status)
					// Untrusted response bodies must not be included in errors.
					fmt.Fprint(w, token)
				}))
				defer server.Close()
				app, stdout, stderr := testApp(t, "")
				app.Info.Version = "1.0.0"
				app.upgradeReleaseAPIURL = server.URL
				argv := []string{"upgrade", "--check", "--debug"}
				if output == "json" {
					argv = append(argv, "--output", "json")
				}
				if exit := app.Run(argv); exit != ExitNetwork {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
				}
				if strings.Contains(stdout.String()+stderr.String(), token) {
					t.Fatal("error output exposed GITHUB_TOKEN")
				}
				if output == "human" {
					for _, want := range []string{"GitHub", "GITHUB_TOKEN", tc.reset} {
						if !strings.Contains(stderr.String(), want) {
							t.Fatalf("missing %q: %s", want, stderr)
						}
					}
					return
				}
				var got struct {
					Error struct {
						Code        string         `json:"code"`
						HTTPStatus  int            `json:"http_status"`
						Retryable   bool           `json:"retryable"`
						Details     map[string]any `json:"details"`
						Remediation map[string]any `json:"remediation"`
					} `json:"error"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				code := "GITHUB_RATE_LIMITED"
				if tc.name == "other forbidden" {
					code = "RELEASE_DOWNLOAD_FAILED"
				} else if got.Error.Remediation["next_actions"] == nil {
					t.Fatalf("missing remediation: %s", stdout)
				}
				if got.Error.Code != code || !got.Error.Retryable || got.Error.HTTPStatus != tc.status || got.Error.Details == nil {
					t.Fatalf("unexpected error envelope: %s", stdout)
				}
				if tc.reset != "" && got.Error.Details["reset_at"] != tc.reset {
					t.Fatalf("missing reset time: %s", stdout)
				}
			})
		}
	}
}

func TestUpgradeGitHubRetryAfterSeconds(t *testing.T) {
	before := time.Now().UTC()
	failure := githubReleaseAPIError(&http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"120"}}})
	details := failure.Details.(map[string]any)
	reset, err := time.Parse(time.RFC3339, details["reset_at"].(string))
	if err != nil || reset.Before(before.Add(119*time.Second)) || reset.After(time.Now().Add(120*time.Second)) {
		t.Fatalf("invalid retry time: %v (%v)", details["reset_at"], err)
	}
}

func TestUpgradeAPITokenSafety(t *testing.T) {
	const token = "upgrade-test-token"
	t.Setenv("GITHUB_TOKEN", token)
	for _, redirect := range []bool{false, true} {
		app, stdout, stderr := testApp(t, "")
		app.Info.Version = "1.0.0"
		app.upgradeReleaseAPIURL = "https://api.example.test/releases"
		requests := 0
		app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("missing API token")
			}
			if redirect {
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://other.example.test/"}}, Body: http.NoBody, Request: r}, nil
			}
			return nil, errors.New("transport error containing " + token)
		})}
		_, failure := app.latestStableCLIRelease(context.Background())
		if failure == nil || requests != 1 {
			t.Fatalf("failure=%v requests=%d", failure, requests)
		}
		app.writeError(failure, Options{Output: "json"})
		if strings.Contains(fmt.Sprintf("%v %v %s %s", failure, failure.Cause, stdout, stderr), token) {
			t.Fatal("API failure exposed GITHUB_TOKEN")
		}
	}
}
