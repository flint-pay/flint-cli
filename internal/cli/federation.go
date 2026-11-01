package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// This internal credential selects a provider. It is never sent over HTTP or
// saved to the keyring. API requests resolve it anew, including after a 401.
const workloadCredential = "\x00workload_identity"
const exchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
const jwtTokenType = "urn:ietf:params:oauth:token-type:jwt"
const accessTokenType = "urn:ietf:params:oauth:token-type:access_token"

type federationConfig struct {
	Base, Identity, Audience, Scope, Source, File, Endpoint string
	TokenHash                                               [32]byte
}

type federationToken struct {
	Value            string
	Expires, Refresh time.Time
}

type federationFlight struct {
	done  chan struct{}
	token federationToken
	err   *CLIError
}

// Package state is shared by copied Apps (including MCP tool invocations).
// Flights publish both successes and failures to all concurrent waiters.
var federationCache = struct {
	sync.Mutex
	tokens  map[federationConfig]federationToken
	flights map[federationConfig]*federationFlight
}{tokens: make(map[federationConfig]federationToken), flights: make(map[federationConfig]*federationFlight)}

func workloadIdentityID() string { return strings.TrimSpace(os.Getenv("FLINT_WORKLOAD_IDENTITY_ID")) }

func environmentCredentialActive() bool {
	return credentialFromEnvironment() != "" || strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")) != "" || workloadIdentityID() != ""
}

func (a *App) warnAmbiguousCredentials() {
	if credentialFromEnvironment() != "" && strings.TrimSpace(os.Getenv("FLINT_ACCESS_TOKEN")) != "" {
		fmt.Fprintln(a.Stderr, "warning: both FLINT_API_KEY and FLINT_ACCESS_TOKEN are set; FLINT_ACCESS_TOKEN takes precedence.")
	}
}

func (a *App) federationConfig(base string) (federationConfig, *CLIError) {
	c := federationConfig{Identity: workloadIdentityID(), Scope: strings.TrimSpace(os.Getenv("FLINT_OIDC_SCOPE")), Source: strings.TrimSpace(os.Getenv("FLINT_OIDC_SOURCE"))}
	if c.Identity == "" {
		return c, c.failure("FEDERATION_SOURCE_UNAVAILABLE", subjectClaims{}, "Set FLINT_WORKLOAD_IDENTITY_ID before exchanging an OIDC token.")
	}
	var err error
	c.Base, err = validateBaseURL(base)
	if err != nil {
		return c, configError("INVALID_BASE_URL", err.Error(), nil)
	}
	u, _ := url.Parse(c.Base)
	c.Audience = strings.TrimSpace(os.Getenv("FLINT_OIDC_AUDIENCE"))
	if c.Audience == "" {
		c.Audience = u.Scheme + "://" + u.Host
	}
	c.File = strings.TrimSpace(os.Getenv("FLINT_OIDC_TOKEN_FILE"))
	c.Endpoint = os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	c.TokenHash = sha256.Sum256([]byte(os.Getenv("FLINT_OIDC_TOKEN") + "\x00" + os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN") + "\x00" + os.Getenv("IDENTITY_ENDPOINT") + "\x00" + os.Getenv("IDENTITY_HEADER") + "\x00" + os.Getenv("FLINT_AZURE_CLIENT_ID")))
	if c.Source == "" || c.Source == "auto" {
		switch {
		case strings.TrimSpace(os.Getenv("FLINT_OIDC_TOKEN")) != "":
			c.Source = "token"
		case c.File != "":
			c.Source = "token_file"
		case os.Getenv("GITHUB_ACTIONS") == "true" || c.Endpoint != "":
			c.Source = "github_actions"
		case os.Getenv("GITLAB_CI") == "true":
			c.Source = "gitlab_ci"
		case os.Getenv("KUBERNETES_SERVICE_HOST") != "":
			c.Source = "kubernetes"
		case gcpEnvironment():
			c.Source = "gcp"
		case azureEnvironment():
			c.Source = "azure"
		case awsEnvironment():
			c.Source = "aws"
		default:
			c.Source = "auto"
			return c, c.failure("FEDERATION_SOURCE_UNAVAILABLE", subjectClaims{}, "No source is available. Tried FLINT_OIDC_TOKEN, FLINT_OIDC_TOKEN_FILE, GitHub Actions (permissions: id-token: write), GitLab CI (id_tokens:), Kubernetes (KUBERNETES_SERVICE_HOST and a Flint projected token), GCP (K_SERVICE, FUNCTION_TARGET, or GCE_METADATA_HOST), Azure (IDENTITY_ENDPOINT with IDENTITY_HEADER), and AWS (Lambda or ECS execution markers). Configure the platform environment. On VMs, set FLINT_OIDC_SOURCE explicitly to gcp, azure, or aws.")
		}
	}
	switch c.Source {
	case "token", "token_file", "github_actions", "gitlab_ci", "kubernetes", "gcp", "azure", "aws":
	case "none":
		return c, c.failure("FEDERATION_SOURCE_UNAVAILABLE", subjectClaims{}, "FLINT_OIDC_SOURCE=none disables OIDC sources. Unset FLINT_WORKLOAD_IDENTITY_ID to use a saved login.")
	default:
		return c, c.failure("FEDERATION_SOURCE_UNAVAILABLE", subjectClaims{}, "Unknown FLINT_OIDC_SOURCE; use auto, none, token, token_file, github_actions, gitlab_ci, kubernetes, gcp, azure, or aws.")
	}
	return c, nil
}

// Remote diagnostics may echo credentials. Remove full tokens and token-shaped
// strings before retaining any server description or unverified claim in errors.
var federationSecretPattern = regexp.MustCompile(`(?:fwat_|fcat_|fcrt_|flint_test_|flint_live_)[A-Za-z0-9_.-]+|eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func federationSafe(value string, secrets ...string) string {
	for _, secret := range append(secrets, os.Getenv("FLINT_OIDC_TOKEN"), os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"), os.Getenv("FLINT_ACCESS_TOKEN"), os.Getenv("FLINT_API_KEY")) {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	value = federationSecretPattern.ReplaceAllString(value, "[REDACTED]")
	if len(value) > 2048 {
		value = value[:2048]
	}
	return strings.Map(func(r rune) rune {
		if (r < 32 && r != '\n') || (r >= 127 && r <= 159) {
			return -1
		}
		return r
	}, value)
}

type subjectClaims struct {
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Audience json.RawMessage `json:"aud"`
	Expiry   int64           `json:"exp"`
}

func (c federationConfig) failure(code string, claims subjectClaims, message string, secrets ...string) *CLIError {
	return configError(code, federationSafe(fmt.Sprintf("Workload identity %q, source %s (iss=%q sub=%q aud=%s): %s", c.Identity, c.Source, claims.Issuer, claims.Subject, claims.Audience, message), secrets...), nil)
}

func (a *App) validateSubject(c federationConfig, token string) (subjectClaims, *CLIError) {
	var claims subjectClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(token) > 8<<10 {
		return claims, c.failure("OIDC_TOKEN_REQUEST_FAILED", claims, "The source did not return a JWT of at most 8 KiB.", token)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return claims, c.failure("OIDC_TOKEN_REQUEST_FAILED", claims, "The source returned invalid JWT claims.", token)
	}
	var audience string
	var audiences []string
	matches := false
	if json.Unmarshal(claims.Audience, &audience) == nil {
		matches = audience == c.Audience
	} else if json.Unmarshal(claims.Audience, &audiences) == nil {
		matches = len(audiences) == 1 && audiences[0] == c.Audience
	}
	if !matches {
		return claims, c.failure("OIDC_AUDIENCE_MISMATCH", claims, fmt.Sprintf("Expected audience %q. Request a token for this audience with FLINT_OIDC_AUDIENCE.", c.Audience), token)
	}
	if claims.Expiry <= a.Now().Unix() {
		return claims, c.failure("OIDC_TOKEN_REQUEST_FAILED", claims, "The subject token is expired or missing exp. Request a fresh token.", token)
	}
	return claims, nil
}

// Neither provider requests nor token exchange use environment proxies or
// redirects. Keep the API client's transport separate from this credential path.
var federationHTTPClient = &http.Client{
	Timeout:       10 * time.Second,
	Transport:     func() *http.Transport { t := http.DefaultTransport.(*http.Transport).Clone(); t.Proxy = nil; return t }(),
	CheckRedirect: rejectAPIRedirect,
}

// Compare both lexical paths and symlink targets, falling back to the cleaned
// absolute path when a file does not exist or cannot be resolved.
func federationFilePaths(path string) (string, string) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = filepath.Clean(path)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		resolved = absolute
	}
	return absolute, resolved
}

func foreignFederationFile(path string) string {
	absolute, resolved := federationFilePaths(path)
	for _, source := range []struct{ name, path string }{
		{"AWS_WEB_IDENTITY_TOKEN_FILE", os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")},
		{"AZURE_FEDERATED_TOKEN_FILE", os.Getenv("AZURE_FEDERATED_TOKEN_FILE")},
		{"the default Kubernetes service account token", "/var/run/secrets/kubernetes.io/serviceaccount/token"},
	} {
		if source.path == "" {
			continue
		}
		foreignAbsolute, foreignResolved := federationFilePaths(source.path)
		if absolute == foreignAbsolute || resolved == foreignResolved {
			return source.name
		}
	}
	return ""
}

func (c federationConfig) subjectToken(ctx context.Context) (string, *CLIError) {
	unavailable := func(message string) (string, *CLIError) {
		return "", c.failure("FEDERATION_SOURCE_UNAVAILABLE", subjectClaims{}, message)
	}
	switch c.Source {
	case "gcp", "azure", "aws":
		return c.cloudSubjectToken(ctx)
	case "token", "gitlab_ci":
		if token := strings.TrimSpace(os.Getenv("FLINT_OIDC_TOKEN")); token != "" {
			return token, nil
		}
		if c.Source == "gitlab_ci" {
			return unavailable("Set a GitLab ID token with YAML:\nid_tokens:\n  FLINT_OIDC_TOKEN:\n    aud: " + c.Audience)
		}
		return unavailable("Set FLINT_OIDC_TOKEN to an OIDC JWT.")
	case "token_file", "kubernetes":
		path := c.File
		if c.Source == "kubernetes" {
			if os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
				return unavailable("KUBERNETES_SERVICE_HOST is required for a Kubernetes projected token.")
			}
			if path == "" {
				path = "/var/run/secrets/flint/token"
			}
		}
		if path == "" {
			return unavailable("Set FLINT_OIDC_TOKEN_FILE to a Flint audience token file.")
		}
		if source := foreignFederationFile(path); source != "" {
			return unavailable("Refusing to read " + source + " as a Flint OIDC token. Set FLINT_OIDC_TOKEN_FILE to a separate token file with the Flint audience.")
		}
		f, err := os.Open(path)
		if err != nil {
			return unavailable("The Flint OIDC token file could not be opened. Check FLINT_OIDC_TOKEN_FILE and its permissions.")
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, (8<<10)+1))
		if err != nil || len(raw) > 8<<10 || strings.TrimSpace(string(raw)) == "" {
			return unavailable("The Flint OIDC token file is empty, unreadable, or exceeds 8 KiB.")
		}
		return strings.TrimSpace(string(raw)), nil
	case "github_actions":
		if c.Endpoint == "" {
			return unavailable("GitHub Actions requires permissions: id-token: write; ACTIONS_ID_TOKEN_REQUEST_URL is missing.")
		}
		bearer := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
		if bearer == "" {
			return unavailable("ACTIONS_ID_TOKEN_REQUEST_TOKEN is missing. Grant permissions: id-token: write in GitHub Actions.")
		}
		endpoint, err := url.Parse(c.Endpoint)
		if err != nil {
			return unavailable("ACTIONS_ID_TOKEN_REQUEST_URL is invalid.")
		}
		// The endpoint is provided by the runner; allow HTTP only for loopback tests.
		if _, err := validateBaseURL(endpoint.Scheme + "://" + endpoint.Host); err != nil || endpoint.User != nil || endpoint.Fragment != "" {
			return unavailable("ACTIONS_ID_TOKEN_REQUEST_URL must use HTTPS or loopback HTTP without credentials or a fragment.")
		}
		query := endpoint.Query()
		query.Set("audience", c.Audience)
		endpoint.RawQuery = query.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
		if err != nil {
			return unavailable("ACTIONS_ID_TOKEN_REQUEST_URL is invalid.")
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := federationHTTPClient.Do(req)
		if err != nil {
			return "", c.failure("OIDC_TOKEN_REQUEST_FAILED", subjectClaims{}, "The GitHub OIDC token request failed.")
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
		var body struct {
			Value string `json:"value"`
		}
		if err != nil || len(raw) > 64<<10 || resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &body) != nil || body.Value == "" {
			return "", c.failure("OIDC_TOKEN_REQUEST_FAILED", subjectClaims{}, "GitHub did not return an OIDC token. Check permissions: id-token: write.")
		}
		return body.Value, nil
	}
	return unavailable("The configured OIDC source is unavailable.")
}

func (a *App) exchangeWorkloadToken(ctx context.Context, c federationConfig) (federationToken, *CLIError) {
	subject, e := c.subjectToken(ctx)
	if e != nil {
		return federationToken{}, e
	}
	claims, e := a.validateSubject(c, subject)
	if e != nil {
		return federationToken{}, e
	}
	form := url.Values{"grant_type": {exchangeGrant}, "subject_token": {subject}, "subject_token_type": {jwtTokenType}, "requested_token_type": {accessTokenType}, "workload_identity_id": {c.Identity}}
	if c.Scope != "" {
		form.Set("scope", c.Scope)
	}
	for attempt := 1; ; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/v1/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "flintpay-cli/"+a.Info.Version)
		resp, err := federationHTTPClient.Do(req)
		if err != nil {
			return federationToken{}, c.failure("FEDERATION_EXCHANGE_FAILED", claims, "Could not reach the configured Flint token endpoint.", subject)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
		resp.Body.Close()
		if retryableErrorStatus(resp.StatusCode) && retryRequest(ctx, attempt, resp.Header.Get("Retry-After")) {
			continue
		}
		var body struct {
			AccessToken     string `json:"access_token"`
			TokenType       string `json:"token_type"`
			IssuedTokenType string `json:"issued_token_type"`
			ExpiresIn       int64  `json:"expires_in"`
			Identity        string `json:"workload_identity_id"`
			Error           string `json:"error"`
			Description     string `json:"error_description"`
		}
		if err != nil || len(raw) > 64<<10 || json.Unmarshal(raw, &body) != nil {
			return federationToken{}, c.failure("FEDERATION_EXCHANGE_FAILED", claims, "Flint returned an unreadable token response.", subject)
		}
		if resp.StatusCode != http.StatusOK {
			e := c.failure("FEDERATION_EXCHANGE_FAILED", claims, fmt.Sprintf("Flint returned HTTP %d: %s: %s", resp.StatusCode, body.Error, body.Description), subject, body.AccessToken)
			e.HTTPStatus = resp.StatusCode
			if delay, ok := parseRetryAfter(resp.Header.Get("Retry-After")); ok {
				ms := delay.Milliseconds()
				e.RetryAfterMs = &ms
			}
			return federationToken{}, e
		}
		if !strings.HasPrefix(body.AccessToken, "fwat_") || !validOAuthToken(body.AccessToken) || !strings.EqualFold(body.TokenType, "Bearer") || body.IssuedTokenType != accessTokenType || body.Identity != c.Identity || body.ExpiresIn < 1 || body.ExpiresIn > 3600 {
			return federationToken{}, c.failure("FEDERATION_EXCHANGE_FAILED", claims, "Flint returned invalid workload access token metadata.", subject, body.AccessToken)
		}
		now := a.Now()
		expires := now.Add(time.Duration(body.ExpiresIn) * time.Second)
		if subjectExpiry := time.Unix(claims.Expiry, 0); subjectExpiry.Before(expires) {
			expires = subjectExpiry
		}
		lifetime := expires.Sub(now)
		if lifetime <= 0 {
			return federationToken{}, c.failure("FEDERATION_EXCHANGE_FAILED", claims, "The subject token expired during exchange.", subject, body.AccessToken)
		}
		refresh := expires.Add(-max(60*time.Second, lifetime/5))
		if !refresh.After(now) {
			// Short-lived tokens still need a positive cache interval, without
			// reusing them inside the final 30-second safety margin.
			usable := max(0, lifetime-30*time.Second)
			refresh = now.Add(min(usable, max(time.Second, usable/2)))
		}
		return federationToken{Value: body.AccessToken, Expires: expires, Refresh: refresh}, nil
	}
}

func (a *App) workloadToken(ctx context.Context, base string, rejected string) (string, *CLIError) {
	c, e := a.federationConfig(base)
	if e != nil {
		return "", e
	}
	federationCache.Lock()
	if cached, ok := federationCache.tokens[c]; ok {
		if cached.Value == rejected {
			delete(federationCache.tokens, c)
		} else if a.Now().Before(cached.Refresh) {
			federationCache.Unlock()
			return cached.Value, nil
		}
	}
	if flight, ok := federationCache.flights[c]; ok {
		federationCache.Unlock()
		select {
		case <-ctx.Done():
			return "", c.failure("FEDERATION_EXCHANGE_FAILED", subjectClaims{}, "Token exchange canceled.")
		case <-flight.done:
			return flight.token.Value, copyFederationError(flight.err)
		}
	}
	flight := &federationFlight{done: make(chan struct{})}
	federationCache.flights[c] = flight
	federationCache.Unlock()
	token, e := a.exchangeWorkloadToken(ctx, c)
	federationCache.Lock()
	flight.token, flight.err = token, e
	if e == nil {
		federationCache.tokens[c] = token
	}
	delete(federationCache.flights, c)
	close(flight.done)
	federationCache.Unlock()
	return token.Value, copyFederationError(e)
}

// Keep API error envelopes useful without allowing an echoed bearer into output.
func redactFederationError(e *CLIError, token string) *CLIError {
	if e == nil {
		return nil
	}
	e.Message = federationSafe(e.Message, token)
	e.Code = federationSafe(e.Code, token)
	e.Type = federationSafe(e.Type, token)
	e.Param = federationSafe(e.Param, token)
	e.RequestID = federationSafe(e.RequestID, token)
	e.Cause = nil
	if raw, err := json.Marshal(e.Details); err == nil {
		var details any
		if decodeJSONNumbers([]byte(federationSafe(string(raw), token)), &details) == nil {
			e.Details = details
		} else {
			e.Details = nil
		}
	}
	return e
}

// Request handling annotates CLIError values, so waiters need independent copies.
func copyFederationError(e *CLIError) *CLIError {
	if e == nil {
		return nil
	}
	copy := *e
	return &copy
}
