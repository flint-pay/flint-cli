package cli

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestHeadlessSignupSavesCredentialInPrivateFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"missing session bus", &exec.Error{Name: "dbus-launch", Err: exec.ErrNotFound}},
		{"missing Secret Service", dbus.Error{Name: "org.freedesktop.DBus.Error.ServiceUnknown"}},
		{"locked collection", errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/login'")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				signupFlowSuccess(t, w, r)
			}))
			defer server.Close()
			t.Setenv("FLINT_BASE_URL", server.URL)
			t.Setenv("FLINT_API_KEY", "")
			t.Setenv("FLINT_ACCESS_TOKEN", "")
			t.Setenv("FLINT_WORKLOAD_IDENTITY_ID", "")
			t.Setenv("FLINT_CREDENTIAL_STORE", "")
			dir, workdir := t.TempDir(), t.TempDir()
			start, out, stderr := continuationTestApp(t, dir, workdir)
			exit, result := runJSON(t, start, out, append(append([]string{}, signupFlowStartArgs...), "--no-input")...)
			command, _ := valueAt(result, "data.next_command").(string)
			if exit != ExitOK || !strings.Contains(command, "flint signup --verification-code") ||
				strings.Contains(command, "--credential-store") || stderr.Len() != 0 {
				t.Fatalf("start=%d next command=%q stderr=%s", exit, command, stderr)
			}

			finish, out, stderr := continuationTestApp(t, dir, workdir)
			writes := 0
			finish.StoreCredential = func(string, string) error {
				writes++
				return tc.err
			}
			exit, result = runJSON(t, finish, out, "signup", "--verification-code", "123456", "--no-input")
			command, _ = valueAt(result, "data.next_command").(string)
			if exit != ExitOK || writes != 1 || valueAt(result, "data.credential_saved") != true ||
				!strings.Contains(command, "--credential-store file") {
				t.Fatalf("finish=%d writes=%d credential_saved=%v next command=%q", exit, writes, valueAt(result, "data.credential_saved"), command)
			}
			path, err := finish.credentialFilePath()
			if err != nil {
				t.Fatal(err)
			}
			notice := stderr.String()
			if !strings.HasPrefix(notice, "Credential saved to "+path+" with owner-only permissions.") ||
				strings.Count(notice, "\n") != 1 {
				t.Fatalf("unexpected file notice: %q", notice)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Fatalf("credential file permissions = %o", info.Mode().Perm())
			}
			next, _, _ := continuationTestApp(t, dir, workdir)
			value, source, err := next.resolveCredential("default")
			if err != nil || source != "file" || value != "flint_test_synthetic_new" {
				t.Fatalf("saved credential source = %s, error = %v", source, err)
			}
		})
	}
}

func TestSignupExplicitKeyringFailureSuggestsSignupRetry(t *testing.T) {
	revoked := false
	server := signupCompensationTestServer(t, &revoked)
	defer server.Close()
	app, out, stderr := testApp(t, server.URL)
	t.Setenv("FLINT_API_KEY", "")
	app.StoreCredential = func(string, string) error {
		return errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/login'")
	}
	app.DeleteCredential = func(string) error {
		t.Error("a keyring that failed to unlock has no new credential to delete")
		return nil
	}
	args := append(append([]string{}, signupFlowStartArgs...), "--verification-code", "123456", "--credential-store", "keyring", "--no-input")
	exit, result := runJSON(t, app, out, args...)
	if exit != ExitAuth || !revoked || stderr.Len() != 0 || valueAt(result, "error.code") != "KEYCHAIN_WRITE_FAILED" {
		t.Fatalf("exit=%d revoked=%t stdout=%s stderr=%s", exit, revoked, out, stderr)
	}
	if !strings.Contains(out.String(), "flint signup --verification-code") ||
		!strings.Contains(out.String(), "--credential-store file") || strings.Contains(out.String(), "flint login") {
		t.Fatalf("signup retry is missing: %s", out)
	}
}
