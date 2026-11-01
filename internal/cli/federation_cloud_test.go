package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Route real HTTP requests to a local fake without adding a production endpoint
// override for metadata. Preserve the production proxy and redirect policies.
func cloudTestHTTP(t *testing.T, server *httptest.Server) {
	t.Helper()
	original := federationHTTPClient
	client := *original
	transport := original.Transport.(*http.Transport).Clone()
	if transport.Proxy != nil || client.CheckRedirect == nil || client.Timeout > 10*time.Second {
		t.Fatal("unsafe identity HTTP policy")
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	client.Transport = transport
	federationHTTPClient = &client
	t.Cleanup(func() { federationHTTPClient = original; transport.CloseIdleConnections() })
}

func cloudTestAWS(t *testing.T) {
	t.Helper()
	for _, name := range []string{"AWS_PROFILE", "AWS_DEFAULT_PROFILE", "AWS_ROLE_ARN", "AWS_ROLE_SESSION_NAME", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_ENDPOINT_URL", "AWS_SESSION_TOKEN"} {
		t.Setenv(name, "")
	}
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_ACCESS_KEY_ID", "synthetic-access-id")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic-secret")
	t.Setenv("AWS_REGION", "us-east-2")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_ENDPOINT_URL_STS", "http://sts.example.test")
}

func cloudTestMarker(t *testing.T, source string) {
	t.Helper()
	switch source {
	case "gcp":
		t.Setenv("K_SERVICE", "synthetic-service")
	case "azure":
		t.Setenv("IDENTITY_ENDPOINT", "http://identity.example.test/token?existing=yes")
		t.Setenv("IDENTITY_HEADER", "synthetic-identity-header")
	case "aws":
		t.Setenv("AWS_EXECUTION_ENV", "AWS_Lambda_go1.x")
	}
}

func TestCloudFederationRequests(t *testing.T) {
	for _, platform := range []string{"gcp", "azure", "azure_imds", "aws"} {
		for _, result := range []string{"success", "success_explicit", "http_failure", "redirect", "wrong_audience", "expired", "malformed", "empty", "oversized"} {
			t.Run(platform+"/"+result, func(t *testing.T) {
				var identities, exchanges, redirects atomic.Int32
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirects.Add(1) }))
				defer target.Close()
				audience := "api://synthetic-merchant"
				token := subjectJWT(audience, time.Now().Add(time.Hour), "synthetic-cloud-subject")
				switch result {
				case "wrong_audience":
					token = subjectJWT("api://foreign", time.Now().Add(time.Hour), "synthetic-cloud-subject")
				case "expired":
					token = subjectJWT(audience, time.Now().Add(-time.Minute), "synthetic-cloud-subject")
				case "malformed":
					token = "synthetic-invalid-token"
				case "empty":
					token = ""
				case "oversized":
					token = strings.Repeat("x", 65<<10)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/oauth/token" {
						exchanges.Add(1)
						_ = r.ParseForm()
						if r.Form.Get("subject_token") != token || r.Form.Get("workload_identity_id") != "wid_synthetic" {
							t.Error("incorrect exchange")
						}
						writeExchange(w, 1)
						return
					}
					identities.Add(1)
					query := r.URL.Query()
					switch platform {
					case "gcp":
						if r.Host != "metadata.google.internal" || r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" || query.Get("audience") != audience || query.Get("format") != "full" || r.Header.Get("Metadata-Flavor") != "Google" || r.Method != "GET" {
							t.Error("incorrect GCP identity request")
						}
					case "azure", "azure_imds":
						if query.Get("resource") != audience || query.Get("client_id") != "synthetic-user-identity" || r.Method != "GET" {
							t.Error("incorrect Azure identity request")
						}
						if platform == "azure" && (r.Header.Get("X-IDENTITY-HEADER") != "synthetic-identity-header" || query.Get("api-version") != "2019-08-01" || query.Get("existing") != "yes") {
							t.Error("incorrect Azure App Service headers or query")
						}
						if platform == "azure_imds" && (r.Host != "169.254.169.254" || r.URL.Path != "/metadata/identity/oauth2/token" || r.Header.Get("Metadata") != "true" || query.Get("api-version") != "2018-02-01" || r.Header.Get("X-IDENTITY-HEADER") != "") {
							t.Error("incorrect Azure IMDS request")
						}
					case "aws":
						_ = r.ParseForm()
						if r.Method != "POST" || r.Form.Get("Action") != "GetWebIdentityToken" || r.Form.Get("Audience.member.1") != audience || r.Form.Get("SigningAlgorithm") != "ES384" || !strings.Contains(r.Header.Get("Authorization"), "/us-east-2/sts/aws4_request") {
							t.Error("incorrect signed AWS outbound federation request")
						}
					}
					if result == "redirect" {
						http.Redirect(w, r, target.URL+"/"+url.PathEscape(token), http.StatusFound)
						return
					}
					if result == "http_failure" {
						w.WriteHeader(http.StatusBadRequest)
					}
					switch platform {
					case "gcp":
						fmt.Fprint(w, token)
					case "azure", "azure_imds":
						_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token})
					case "aws":
						w.Header().Set("Content-Type", "text/xml")
						fmt.Fprintf(w, `<GetWebIdentityTokenResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetWebIdentityTokenResult><WebIdentityToken>%s</WebIdentityToken></GetWebIdentityTokenResult></GetWebIdentityTokenResponse>`, token)
					}
				}))
				defer server.Close()
				app := federationApp(t, server.URL)
				t.Setenv("FLINT_OIDC_TOKEN", "")
				t.Setenv("FLINT_OIDC_AUDIENCE", audience)
				t.Setenv("FLINT_AZURE_CLIENT_ID", "synthetic-user-identity")
				cloudTestAWS(t)
				cloudTestMarker(t, platform)
				if platform == "azure_imds" || result == "success_explicit" {
					t.Setenv("FLINT_OIDC_SOURCE", strings.Split(platform, "_")[0])
				}
				cloudTestHTTP(t, server)
				for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
					t.Setenv(key, target.URL)
				}
				t.Setenv("NO_PROXY", "")
				// Exercise auto discovery through exchange and its shared aud/exp
				// checks. No invalid subject may reach the exchange server.
				_, e := app.workloadToken(context.Background(), server.URL, "")
				if strings.HasPrefix(result, "success") {
					if e != nil || exchanges.Load() != 1 {
						t.Fatalf("exchange count=%d error=%v", exchanges.Load(), e)
					}
				} else {
					code := "OIDC_TOKEN_REQUEST_FAILED"
					if result == "wrong_audience" {
						code = "OIDC_AUDIENCE_MISMATCH"
					}
					if e == nil || e.Code != code || exchanges.Load() != 0 {
						t.Fatalf("exchange count=%d error=%v", exchanges.Load(), e)
					}
					if (token != "" && strings.Contains(e.Message, token)) || strings.Contains(e.Message, "synthetic-identity-header") || strings.Contains(e.Message, "synthetic-secret") {
						t.Fatal("identity error leaked a credential")
					}
				}
				if identities.Load() != 1 || redirects.Load() != 0 {
					t.Fatalf("identity=%d redirect/proxy=%d", identities.Load(), redirects.Load())
				}
			})
		}
	}
}

func TestCloudSourceMarkers(t *testing.T) {
	app := federationApp(t, "https://api.example.test")
	t.Setenv("FLINT_OIDC_TOKEN", "")
	cloudTestAWS(t)
	t.Setenv("FLINT_OIDC_AUDIENCE", "https://api.example.test")
	token := subjectJWT("https://api.example.test", time.Now().Add(time.Hour), "synthetic-explicit-subject")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.Host {
		case "metadata.google.internal":
			fmt.Fprint(w, token)
		case "169.254.169.254":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": token})
		default:
			fmt.Fprintf(w, `<GetWebIdentityTokenResponse><GetWebIdentityTokenResult><WebIdentityToken>%s</WebIdentityToken></GetWebIdentityTokenResult></GetWebIdentityTokenResponse>`, token)
		}
	}))
	defer server.Close()
	cloudTestHTTP(t, server)
	for _, source := range []string{"gcp", "azure", "aws"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("FLINT_OIDC_SOURCE", "auto")
			before := requests.Load()
			if _, e := app.workloadToken(context.Background(), "https://api.example.test", ""); e == nil || !strings.Contains(e.Message, "No source is available") {
				t.Fatal(e)
			}
			if requests.Load() != before {
				t.Fatal("auto mode probed metadata without a marker")
			}
			t.Setenv("FLINT_OIDC_SOURCE", source)
			c, e := app.federationConfig("https://api.example.test")
			if e != nil {
				t.Fatal(e)
			}
			if got, e := c.subjectToken(context.Background()); e != nil || got != token {
				t.Fatalf("explicit %s failed without a marker: %v", source, e)
			}
			cloudTestMarker(t, source)
			t.Setenv("FLINT_OIDC_SOURCE", "auto")
			if c, e = app.federationConfig("https://api.example.test"); e != nil || c.Source != source {
				t.Fatal(c, e)
			}
		})
	}
	// Incomplete identity endpoint and generic credentials aren't platform proof.
	for key, value := range map[string]string{"IDENTITY_ENDPOINT": "http://identity.example.test", "AZURE_CLIENT_ID": "synthetic-id", "AWS_EXECUTION_ENV": "unrelated", "AWS_WEB_IDENTITY_TOKEN_FILE": "must-not-read", "AZURE_FEDERATED_TOKEN_FILE": "must-not-read"} {
		t.Setenv(key, value)
	}
	if _, e := app.federationConfig("https://api.example.test"); e == nil {
		t.Fatal("foreign credentials selected a cloud source")
	}
	// Every documented marker selects its source, and the first source wins.
	for _, pair := range [][2]string{{"GCE_METADATA_HOST", "gcp"}, {"FUNCTION_TARGET", "gcp"}, {"AWS_LAMBDA_FUNCTION_NAME", "aws"}, {"ECS_CONTAINER_METADATA_URI", "aws"}, {"ECS_CONTAINER_METADATA_URI_V4", "aws"}} {
		t.Run(pair[0], func(t *testing.T) {
			t.Setenv("IDENTITY_ENDPOINT", "")
			t.Setenv(pair[0], "synthetic-marker")
			if c, e := app.federationConfig("https://api.example.test"); e != nil || c.Source != pair[1] {
				t.Fatal(c, e)
			}
		})
	}
	cloudTestMarker(t, "aws")
	cloudTestMarker(t, "azure")
	cloudTestMarker(t, "gcp")
	if c, _ := app.federationConfig("https://api.example.test"); c.Source != "gcp" {
		t.Fatal(c)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "synthetic-host")
	if c, _ := app.federationConfig("https://api.example.test"); c.Source != "kubernetes" {
		t.Fatal(c)
	}
}

func TestAWSRefusesInboundIdentityFiles(t *testing.T) {
	for _, profile := range []bool{false, true} {
		t.Run(fmt.Sprint("profile=", profile), func(t *testing.T) {
			app := federationApp(t, "https://api.example.test")
			cloudTestAWS(t)
			cloudTestMarker(t, "aws")
			t.Setenv("FLINT_OIDC_TOKEN", "")
			t.Setenv("AWS_ACCESS_KEY_ID", "")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "")
			// A readable foreign token must never be submitted to STS.
			path := filepath.Join(t.TempDir(), "foreign-token")
			if err := os.WriteFile(path, []byte(subjectJWT("https://sts.amazonaws.com", time.Now().Add(time.Hour), "foreign-subject")), 0600); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusBadRequest) }))
			defer server.Close()
			cloudTestHTTP(t, server)
			if profile {
				config := filepath.Join(t.TempDir(), "config")
				if err := os.WriteFile(config, []byte("[default]\nrole_arn = arn:aws:iam::000000000000:role/synthetic\nweb_identity_token_file = "+path+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("AWS_CONFIG_FILE", config)
			} else {
				t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", path)
				t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::000000000000:role/synthetic")
			}
			c, e := app.federationConfig("https://api.example.test")
			if e != nil {
				t.Fatal(e)
			}
			if _, e = c.subjectToken(context.Background()); e == nil || e.Code != "OIDC_TOKEN_REQUEST_FAILED" || strings.Contains(e.Message, path) {
				t.Fatal(e)
			}
			if requests.Load() != 0 {
				t.Fatal("foreign web identity was submitted to STS")
			}
		})
	}
}

func TestAzureConfigurationAndCloudCancellation(t *testing.T) {
	app := federationApp(t, "https://api.example.test")
	t.Setenv("FLINT_OIDC_TOKEN", "")
	cloudTestMarker(t, "azure")
	c, e := app.federationConfig("https://api.example.test")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.subjectToken(context.Background()); e == nil || e.Code != "FEDERATION_SOURCE_UNAVAILABLE" || !strings.Contains(e.Message, "Entra app ID URI") {
		t.Fatal(e)
	}
	t.Setenv("FLINT_OIDC_AUDIENCE", "api://synthetic-merchant")
	for _, missing := range []string{"IDENTITY_ENDPOINT", "IDENTITY_HEADER"} {
		t.Run("missing_"+missing, func(t *testing.T) {
			t.Setenv(missing, "")
			if _, e := c.subjectToken(context.Background()); e == nil || e.Code != "FEDERATION_SOURCE_UNAVAILABLE" || !strings.Contains(e.Message, "together") {
				t.Fatal(e)
			}
		})
	}
	for _, endpoint := range []string{"http://user:synthetic-secret@identity.example.test", "file:///token", "http://identity.example.test/#synthetic-secret", "http://%"} {
		t.Setenv("IDENTITY_ENDPOINT", endpoint)
		if _, e = c.subjectToken(context.Background()); e == nil || e.Code != "FEDERATION_SOURCE_UNAVAILABLE" || strings.Contains(e.Message, "synthetic-secret") {
			t.Fatal(e)
		}
	}
	t.Setenv("IDENTITY_ENDPOINT", "http://identity.example.test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("canceled request reached identity endpoint") }))
	defer server.Close()
	cloudTestHTTP(t, server)
	cloudTestAWS(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, source := range []string{"gcp", "azure", "aws"} {
		cloudTestMarker(t, source)
		c.Source = source
		if _, e = c.subjectToken(ctx); e == nil || e.Code != "OIDC_TOKEN_REQUEST_FAILED" {
			t.Fatal(source, e)
		}
	}
}
