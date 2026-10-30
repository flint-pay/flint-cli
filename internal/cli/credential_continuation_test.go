package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A separate App and only shared on-disk state model a new CLI process.
func continuationTestApp(t *testing.T, dir, workdir string) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	a := New(BuildInfo{Version: "test", APIVersion: "2026-02-01"})
	out, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	a.ConfigDir, a.WorkingDir = dir, workdir
	a.Stdout, a.Stderr = out, stderr
	a.Stdin = strings.NewReader("")
	a.IsTTY = func() bool { return false }
	a.LoadCredential = func(string) (string, error) {
		t.Error("continuation read unavailable keyring")
		return "", errors.New("keyring unavailable")
	}
	a.StoreCredential = func(string, string) error {
		t.Error("continuation wrote unavailable keyring")
		return errors.New("keyring unavailable")
	}
	a.DeleteCredential = func(string) error {
		t.Error("continuation deleted unavailable keyring")
		return errors.New("keyring unavailable")
	}
	return a, out, stderr
}

func continuationTestArgs(t *testing.T, result map[string]any, baseURL, mode string) []string {
	t.Helper()
	command, _ := valueAt(result, "data.next_command").(string)
	if !strings.Contains(command, " --credential-store file") {
		t.Fatalf("continuation lost backend: %s", command)
	}
	prefix := "FLINT_BASE_URL='" + baseURL + "' "
	command = strings.TrimPrefix(command, prefix)
	// These fixtures use the default profile and contain no quoted arguments.
	args := strings.Fields(strings.ReplaceAll(command, "<code>", "123456"))
	if len(args) < 2 || args[0] != "flint" {
		t.Fatalf("invalid continuation: %s", command)
	}
	args = args[1:]
	if mode == "metadata" {
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--credential-store" {
				args = append(args[:i], args[i+2:]...)
				break
			}
		}
	}
	return append(args, "--no-input", "--output", "json")
}

func TestSignupCredentialStoreContinuation(t *testing.T) {
	for _, mode := range []string{"printed", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { signupFlowSuccess(t, w, r) }))
			defer server.Close()
			t.Setenv("FLINT_BASE_URL", server.URL)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "")
			t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
			t.Setenv("FLINT_CREDENTIAL_STORE", "")
			dir, workdir := t.TempDir(), t.TempDir()
			a, out, stderr := continuationTestApp(t, dir, workdir)
			argv := append(append([]string{}, signupFlowStartArgs...), "--credential-store", "file", "--no-input")
			exit, result := runJSON(t, a, out, argv...)
			if exit != ExitOK || valueAt(result, "data.status") != "verification_required" {
				t.Fatalf("start=%d %s %s", exit, out, stderr)
			}
			cfg, err := a.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Profiles["default"]
			if p.CredentialStore != "" || (p.CredentialPresent != nil && *p.CredentialPresent) || p.PendingSignup == nil || p.PendingSignup.CredentialStore != "file" {
				t.Fatal("pending signup altered active backend or lost pending backend")
			}
			next, out, stderr := continuationTestApp(t, dir, workdir)
			if exit := next.Run(continuationTestArgs(t, result, server.URL, mode)); exit != ExitOK {
				t.Fatalf("resume=%d %s %s", exit, out, stderr)
			}
			value, source, err := next.resolveCredential("default")
			if err != nil || value != "flint_test_synthetic_new" || source != "file" {
				t.Fatal("resumed signup did not save to file", source, err)
			}
		})
	}
}

func TestLoginCredentialStoreContinuation(t *testing.T) {
	for _, mode := range []string{"printed", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			devices, polls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case loginDevicePath:
					devices++
					writeTestDevice(w)
				case loginTokenPath:
					polls++
					writeTestTokens(w, testOAuthAccess, testOAuthRefresh)
				case "/v1/auth-context":
					fmt.Fprint(w, oauthAuthJSON("sandbox"))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer server.Close()
			t.Setenv("FLINT_BASE_URL", server.URL)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "")
			t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
			t.Setenv("FLINT_CREDENTIAL_STORE", "")
			dir, workdir := t.TempDir(), t.TempDir()
			a, out, stderr := continuationTestApp(t, dir, workdir)
			exit, result := runJSON(t, a, out, "login", "--no-wait", "--credential-store", "file")
			if exit != ExitOK || valueAt(result, "data.status") != "approval_pending" {
				t.Fatalf("start=%d %s %s", exit, out, stderr)
			}
			cfg, err := a.loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Profiles["default"].CredentialStore != "" {
				t.Fatal("pending login altered active backend")
			}
			pending, err := a.loadPendingLogin("default", server.URL)
			if err != nil || pending == nil || pending.CredentialStore != "file" {
				t.Fatal("pending login lost backend", err)
			}
			next, out, stderr := continuationTestApp(t, dir, workdir)
			if exit := next.Run(continuationTestArgs(t, result, server.URL, mode)); exit != ExitOK {
				t.Fatalf("resume=%d %s %s", exit, out, stderr)
			}
			value, source, err := next.resolveCredential("default")
			if err != nil || !isOAuthCredential(value) || source != "file" || devices != 1 || polls != 1 {
				t.Fatal("resumed login did not save to file", source, err, devices, polls)
			}
		})
	}
}

func TestSignupKeyringContinuationOverridesFileEnvironment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { signupFlowSuccess(t, w, r) }))
	defer server.Close()
	t.Setenv("FLINT_BASE_URL", server.URL)
	t.Setenv("FLINT_API_KEY", "")
	t.Setenv("FLINT_ACCESS_TOKEN", "")
	t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
	t.Setenv("FLINT_CREDENTIAL_STORE", "file")
	dir, workdir, stored := t.TempDir(), t.TempDir(), ""
	makeApp := func() (*App, *bytes.Buffer, *bytes.Buffer) {
		a, out, stderr := continuationTestApp(t, dir, workdir)
		a.LoadCredential = func(string) (string, error) { return stored, nil }
		a.StoreCredential = func(_ string, value string) error { stored = value; return nil }
		return a, out, stderr
	}
	a, out, stderr := makeApp()
	argv := append(append([]string{}, signupFlowStartArgs...), "--credential-store", "keyring", "--no-input")
	exit, result := runJSON(t, a, out, argv...)
	command, _ := valueAt(result, "data.next_command").(string)
	if exit != ExitOK || !strings.Contains(command, " --credential-store keyring") {
		t.Fatalf("start=%d command=%s stderr=%s", exit, command, stderr)
	}
	command = strings.TrimPrefix(command, "FLINT_BASE_URL='"+server.URL+"' ")
	args := strings.Fields(strings.ReplaceAll(command, "<code>", "123456"))[1:]
	next, out, stderr := makeApp()
	if exit := next.Run(append(args, "--no-input", "--output", "json")); exit != ExitOK {
		t.Fatalf("resume=%d %s %s", exit, out, stderr)
	}
	cfg, err := next.loadConfig()
	if err != nil || cfg.Profiles["default"].CredentialStore != "keyring" || stored != "flint_test_synthetic_new" {
		t.Fatal("explicit keyring choice lost", err)
	}
	if strings.Contains(stderr.String(), "not encrypted") {
		t.Fatal("continuation silently selected the file backend")
	}
}
