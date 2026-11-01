package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAWSCredentialTransport(t *testing.T) {
	if scenario := os.Getenv("FLINT_TEST_AWS_TRANSPORT"); scenario != "" {
		testAWSCredentialTransport(t, strings.Split(scenario, "/"))
		return
	}
	for _, provider := range []string{"ecs_relative", "ecs_full", "ec2"} {
		for _, result := range []string{"proxy", "307", "308"} {
			if provider == "ecs_relative" && result != "proxy" {
				continue
			}
			t.Run(provider+"/"+result, func(t *testing.T) {
				// ProxyFromEnvironment caches the environment on first use.
				// Isolate each case so earlier tests cannot mask a proxy leak.
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAWSCredentialTransport$")
				cmd.Env = append(os.Environ(), "FLINT_TEST_AWS_TRANSPORT="+provider+"/"+result)
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("credential transport regression: %v\n%s", err, output)
				}
			})
		}
	}
}

func testAWSCredentialTransport(t *testing.T, scenario []string) {
	t.Helper()
	provider, result := scenario[0], scenario[1]
	app := federationApp(t, "https://api.example.test")
	cloudTestAWS(t)
	t.Setenv("FLINT_OIDC_TOKEN", "")
	t.Setenv("FLINT_OIDC_SOURCE", "aws")
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE"} {
		t.Setenv(key, "")
	}
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN", "synthetic-container-authorization")
	credentials := map[string]string{
		"Code": "Success", "AccessKeyId": "synthetic-acquired-id", "SecretAccessKey": "synthetic-acquired-secret",
		"Token": "synthetic-acquired-session", "Expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
	var escaped, metadata, stsRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped.Add(1)
		_ = json.NewEncoder(w).Encode(credentials)
	}))
	defer target.Close()
	_, targetPort, _ := net.SplitHostPort(target.Listener.Addr().String())
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		proxy := ""
		if result == "proxy" {
			proxy = target.URL
		}
		t.Setenv(key, proxy)
	}
	for _, key := range []string{"NO_PROXY", "no_proxy", "REQUEST_METHOD"} {
		t.Setenv(key, "")
	}
	token := subjectJWT("https://api.example.test", time.Now().Add(time.Hour), "synthetic-aws-subject")
	redirect := target.URL + "/forwarded"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			stsRequests.Add(1)
			_ = r.ParseForm()
			if r.Form.Get("Action") != "GetWebIdentityToken" || !strings.Contains(r.Header.Get("Authorization"), "Credential=synthetic-acquired-id/") || r.Header.Get("X-Amz-Security-Token") != credentials["Token"] {
				t.Error("STS did not use the acquired role credentials")
			}
			fmt.Fprintf(w, `<GetWebIdentityTokenResponse><GetWebIdentityTokenResult><WebIdentityToken>%s</WebIdentityToken></GetWebIdentityTokenResult></GetWebIdentityTokenResponse>`, token)
			return
		}
		metadata.Add(1)
		if provider == "ec2" {
			switch r.URL.Path {
			case "/latest/api/token":
				if r.Method != "PUT" {
					t.Error("incorrect IMDS token method")
				}
				w.Header().Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")
				fmt.Fprint(w, "synthetic-imds-token")
				return
			case "/latest/meta-data/iam/security-credentials/":
				fmt.Fprint(w, "synthetic-role")
				return
			case "/latest/meta-data/iam/security-credentials/synthetic-role":
				if r.Header.Get("X-aws-ec2-metadata-token") != "synthetic-imds-token" {
					t.Error("missing IMDSv2 token")
				}
			default:
				t.Errorf("unexpected IMDS path %s", r.URL.Path)
			}
		} else if r.URL.Path != "/ecs-credentials" || r.Header.Get("Authorization") != "synthetic-container-authorization" {
			t.Error("incorrect ECS credential request")
		}
		if result != "proxy" {
			status := http.StatusTemporaryRedirect
			if result == "308" {
				status = http.StatusPermanentRedirect
			}
			http.Redirect(w, r, redirect, status)
			return
		}
		_ = json.NewEncoder(w).Encode(credentials)
	}))
	defer server.Close()
	cloudTestHTTP(t, server)
	// Preserve distinct redirect destinations while mapping link-local metadata
	// and STS to the fake server. A same-host, different-port redirect must not
	// receive either container Authorization or the IMDSv2 token.
	federationHTTPClient.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		_, port, _ := net.SplitHostPort(address)
		if port == targetPort {
			return (&net.Dialer{}).DialContext(ctx, network, target.Listener.Addr().String())
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	switch provider {
	case "ecs_relative":
		t.Setenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI", "/ecs-credentials")
	case "ecs_full":
		endpoint := "http://169.254.170.2/ecs-credentials"
		if result != "proxy" {
			endpoint = server.URL + "/ecs-credentials"
		}
		t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", endpoint)
	case "ec2":
		t.Setenv("AWS_EC2_METADATA_DISABLED", "false")
		endpoint := "http://169.254.169.254"
		if result != "proxy" {
			endpoint = server.URL
		}
		t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", endpoint)
		t.Setenv("AWS_EC2_METADATA_V1_DISABLED", "true")
	}
	c, e := app.federationConfig("https://api.example.test")
	if e != nil {
		t.Fatal(e)
	}
	got, e := c.subjectToken(context.Background())
	if escaped.Load() != 0 {
		t.Fatalf("credential request escaped to proxy/redirect target: %d", escaped.Load())
	}
	if metadata.Load() == 0 {
		t.Fatal("test did not acquire role credentials")
	}
	if result == "proxy" {
		if e != nil || got != token || stsRequests.Load() != 1 {
			t.Fatalf("role credential acquisition failed: %v", e)
		}
	} else if e == nil || e.Code != "OIDC_TOKEN_REQUEST_FAILED" || stsRequests.Load() != 0 {
		t.Fatalf("credential redirect was not refused: %v", e)
	}
	if e != nil {
		for _, secret := range []string{"synthetic-container-authorization", "synthetic-imds-token", credentials["SecretAccessKey"], credentials["Token"], token} {
			if strings.Contains(e.Message, secret) {
				t.Fatal("credential error leaked a token")
			}
		}
	}
}
