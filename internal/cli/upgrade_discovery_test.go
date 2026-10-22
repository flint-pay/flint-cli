package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	const retryAt = "2030-01-02T02:06:05Z"
	reset, _ := time.Parse(time.RFC3339, resetAt)
	retry, _ := time.Parse(time.RFC3339, retryAt)
	timestamp := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)
	for _, tc := range []struct {
		name         string
		status       int
		header       http.Header
		forbidden    bool
		reset        string
		retrySeconds bool
	}{
		{name: "forbidden with remaining quota and reset", status: 403, header: http.Header{"X-Ratelimit-Remaining": {"4999"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}, forbidden: true},
		{name: "forbidden with reset only", status: 403, header: http.Header{"X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}, forbidden: true},
		{name: "primary limit", status: 403, header: http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}, reset: resetAt},
		{name: "secondary limit seconds overrides reset", status: 403, header: http.Header{"Retry-After": {"120"}, "X-Ratelimit-Remaining": {"4999"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}, retrySeconds: true},
		{name: "secondary limit date overrides reset", status: 403, header: http.Header{"Retry-After": {retry.Format(http.TimeFormat)}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}, reset: retryAt},
		{name: "too many requests with retry seconds", status: 429, header: http.Header{"Retry-After": {"120"}}, retrySeconds: true},
		{name: "too many requests with retry date", status: 429, header: http.Header{"Retry-After": {retry.Format(http.TimeFormat)}}, reset: retryAt},
		{name: "too many requests without headers", status: 429, header: http.Header{}},
		{name: "too many requests with reset only", status: 429, header: http.Header{"X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}},
		{name: "invalid retry after does not use primary reset", status: 403, header: http.Header{"Retry-After": {"invalid"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(reset.Unix())}}},
		{name: "unknown reset", status: 403, header: http.Header{"X-Ratelimit-Remaining": {"0"}}},
		{name: "other forbidden", status: 403, header: http.Header{}, forbidden: true},
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
				before := time.Now()
				if exit := app.Run(argv); exit != ExitNetwork {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
				}
				after := time.Now()
				if strings.Contains(stdout.String()+stderr.String(), token) {
					t.Fatal("error output exposed GITHUB_TOKEN")
				}
				var reportedReset string
				if output == "human" {
					want := "GitHub API rate limit reached"
					if tc.forbidden {
						want = "GitHub refused the latest Flint CLI release lookup (HTTP 403)"
						if strings.Contains(stderr.String(), "rate limit") {
							t.Fatalf("forbidden response reported as rate limited: %s", stderr)
						}
					}
					if !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), "GITHUB_TOKEN") {
						t.Fatalf("unexpected human error: %s", stderr)
					}
					reportedReset = timestamp.FindString(stderr.String())
				} else {
					var got struct {
						Error struct {
							Type        string         `json:"type"`
							Code        string         `json:"code"`
							HTTPStatus  int            `json:"http_status"`
							Retryable   bool           `json:"retryable"`
							Details     map[string]any `json:"details"`
							Remediation struct {
								NextActions []struct {
									ReasonMessage string `json:"reason_message"`
								} `json:"next_actions"`
							} `json:"remediation"`
						} `json:"error"`
					}
					if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
						t.Fatal(err)
					}
					code := "GITHUB_RATE_LIMITED"
					if tc.forbidden {
						code = "RELEASE_DOWNLOAD_FAILED"
					} else if got.Error.Type != "rate_limit_error" || len(got.Error.Remediation.NextActions) == 0 {
						t.Fatalf("missing rate limit type or remediation: %s", stdout)
					}
					if got.Error.Code != code || !got.Error.Retryable || got.Error.HTTPStatus != tc.status || got.Error.Details == nil {
						t.Fatalf("unexpected error envelope: %s", stdout)
					}
					reportedReset, _ = got.Error.Details["reset_at"].(string)
					if reportedReset == "" {
						if _, exists := got.Error.Details["reset_at"]; exists {
							t.Fatalf("unexpected reset time: %s", stdout)
						}
					} else if timestamp.FindString(got.Error.Remediation.NextActions[0].ReasonMessage) != reportedReset {
						t.Fatalf("remediation and details disagree on reset time: %s", stdout)
					}
				}
				if tc.retrySeconds {
					retryTime, err := time.Parse(time.RFC3339, reportedReset)
					if err != nil || retryTime.Before(before.Add(119*time.Second)) || retryTime.After(after.Add(120*time.Second)) {
						t.Fatalf("invalid retry time: %q (%v)", reportedReset, err)
					}
				} else if reportedReset != tc.reset {
					t.Fatalf("reset time=%q, want %q", reportedReset, tc.reset)
				}
			})
		}
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
