package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStandaloneUpgradeVerifiesAndReplacesBinary(t *testing.T) {
	oldBinary := []byte("old flint")
	newBinary := []byte("new flint release")
	archive := testUpgradeArchive(t, newBinary)
	digest := sha256.Sum256(archive)
	archiveName := "flint_1.2.0_linux_amd64.tar.gz"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/releases":
			if request.Header.Get("User-Agent") != "flintpay-cli/1.0.0" {
				t.Errorf("User-Agent=%q", request.Header.Get("User-Agent"))
			}
			fmt.Fprint(w, `[
				{"tag_name":"cli/v1.2.0-beta.1","prerelease":true},
				{"tag_name":"cli/v1.1.0"},
				{"tag_name":"cli/v1.2.0"}
			]`)
		case strings.HasSuffix(request.URL.Path, "/checksums.txt"):
			if !strings.Contains(request.URL.EscapedPath(), "/cli%2Fv1.2.0/") {
				t.Errorf("unescaped release tag path: %s", request.URL.EscapedPath())
			}
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(digest[:]), archiveName)
		case strings.HasSuffix(request.URL.Path, "/"+archiveName):
			if !strings.Contains(request.URL.EscapedPath(), "/cli%2Fv1.2.0/") {
				t.Errorf("unescaped release tag path: %s", request.URL.EscapedPath())
			}
			w.Write(archive)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	executable := filepath.Join(t.TempDir(), "flint")
	if err := os.WriteFile(executable, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := testApp(t, "")
	app.Info.Version = "1.0.0"
	app.upgradeReleaseAPIURL = server.URL + "/releases"
	app.upgradeReleaseDownloadURL = server.URL + "/download"
	app.executablePath = func() (string, error) { return executable, nil }
	app.runtimeGOOS = "linux"
	app.runtimeGOARCH = "amd64"
	app.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if strings.HasPrefix(filepath.Base(name), ".flint-upgrade-") && len(args) > 0 && args[0] == "version" {
			return []byte("1.2.0\n"), nil
		}
		return nil, fmt.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
	}

	if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBinary) {
		t.Fatalf("installed binary = %q", got)
	}
	for _, want := range []string{`"status":"upgraded"`, `"previous_version":"1.0.0"`, `"installed_version":"1.2.0"`, `"install_method":"standalone"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output missing %s: %s", want, stdout)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%s", stderr)
	}
}

func TestStandaloneUpgradeChecksumMismatchPreservesBinary(t *testing.T) {
	oldBinary := []byte("old flint")
	archive := testUpgradeArchive(t, []byte("untrusted replacement"))
	archiveName := "flint_1.1.0_darwin_arm64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/releases":
			fmt.Fprint(w, `[{"tag_name":"cli/v1.1.0"}]`)
		case strings.HasSuffix(request.URL.Path, "/checksums.txt"):
			fmt.Fprintf(w, "%064d  %s\n", 0, archiveName)
		case strings.HasSuffix(request.URL.Path, "/"+archiveName):
			w.Write(archive)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	executable := filepath.Join(t.TempDir(), "flint")
	if err := os.WriteFile(executable, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	app, stdout, _ := testApp(t, "")
	app.Info.Version = "1.0.0"
	app.upgradeReleaseAPIURL = server.URL + "/releases"
	app.upgradeReleaseDownloadURL = server.URL + "/download"
	app.executablePath = func() (string, error) { return executable, nil }
	app.runtimeGOOS = "darwin"
	app.runtimeGOARCH = "arm64"

	if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitSoftware {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
	if !strings.Contains(stdout.String(), `"code":"CHECKSUM_MISMATCH"`) {
		t.Fatalf("stdout=%s", stdout)
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, oldBinary) {
		t.Fatalf("binary changed after checksum failure: %q", got)
	}
}

func TestStandaloneUpgradeVersionMismatchPreservesBinary(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "flint")
	oldBinary := []byte("old flint")
	if err := os.WriteFile(executable, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	app, _, _ := testApp(t, "")
	app.runCommand = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("1.0.0\n"), nil
	}
	upgradeErr := app.replaceExecutableAtomically(t.Context(), executable, []byte("wrong release"), "1.1.0")
	if upgradeErr == nil || upgradeErr.Code != "UPGRADE_VERIFICATION_FAILED" {
		t.Fatalf("error=%v", upgradeErr)
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, oldBinary) {
		t.Fatalf("binary changed after version mismatch: %q", got)
	}
}

func TestStandaloneUpgradeCancellationPreservesBinary(t *testing.T) {
	for _, verificationFails := range []bool{false, true} {
		t.Run(fmt.Sprint(verificationFails), func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "flint")
			if err := os.WriteFile(executable, []byte("old flint"), 0o755); err != nil {
				t.Fatal(err)
			}
			app, _, _ := testApp(t, "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			app.runCommand = func(context.Context, string, ...string) ([]byte, error) {
				cancel()
				if verificationFails {
					return nil, ctx.Err()
				}
				return []byte("1.1.0\n"), nil
			}
			upgradeErr := app.replaceExecutableAtomically(ctx, executable, []byte("new flint"), "1.1.0")
			if upgradeErr == nil || upgradeErr.Code != "REQUEST_CANCELED" || !errors.Is(upgradeErr.Cause, context.Canceled) {
				t.Fatalf("error=%v", upgradeErr)
			}
			data, err := os.ReadFile(executable)
			if err != nil || string(data) != "old flint" {
				t.Fatalf("original changed: data=%q error=%v", data, err)
			}
			leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(executable), ".flint-upgrade-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("temporary files remain: %v, %v", leftovers, err)
			}
		})
	}
}

func TestUpgradeUsesOwningPackageManager(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	npmRoot := filepath.Join(root, "node_modules")
	npmBinary := filepath.Join(npmRoot, "@flintpay", "cli-darwin-arm64", "bin", "flint")
	brewCellar := filepath.Join(root, "Cellar", "flint")
	brewBinary := filepath.Join(brewCellar, "1.0.0", "bin", "flint")
	brewPrefix := filepath.Join(root, "opt", "flint")
	for _, test := range []struct {
		name       string
		executable string
		want       []string
	}{
		{name: "npm", executable: npmBinary, want: []string{"npm root -g", "npm install -g @flintpay/cli@1.1.0 --no-audit --no-fund", npmBinary + " version --field data.cli_version --color never"}},
		{name: "homebrew", executable: brewBinary, want: []string{"brew --cellar flint-pay/tap/flint", "brew update", "brew upgrade flint-pay/tap/flint", "brew --prefix flint-pay/tap/flint", filepath.Join(brewPrefix, "bin", "flint") + " version --field data.cli_version --color never"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `[{"tag_name":"cli/v1.1.0"}]`)
			}))
			defer server.Close()
			app, stdout, stderr := testApp(t, "")
			app.Info.Version = "1.0.0"
			app.upgradeReleaseAPIURL = server.URL
			app.executablePath = func() (string, error) { return test.executable, nil }
			app.runtimeGOOS = "darwin"
			var commands []string
			app.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
				commands = append(commands, strings.Join(append([]string{name}, args...), " "))
				if name == "npm" && slices.Equal(args, []string{"root", "-g"}) {
					return []byte(npmRoot + "\n"), nil
				}
				if name == "brew" && slices.Equal(args, []string{"--cellar", "flint-pay/tap/flint"}) {
					return []byte(brewCellar + "\n"), nil
				}
				if name == "brew" && slices.Equal(args, []string{"--prefix", "flint-pay/tap/flint"}) {
					return []byte(brewPrefix + "\n"), nil
				}
				if len(args) > 0 && args[0] == "version" {
					return []byte("1.1.0\n"), nil
				}
				return nil, nil
			}

			if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitOK {
				t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
			}
			if !slices.Equal(commands, test.want) {
				t.Fatalf("commands=%v want=%v", commands, test.want)
			}
		})
	}
}

func TestPackageManagerUpgradeVerifiesCurrentInstallation(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	npmRoot := filepath.Join(root, "node_modules")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"cli/v1.1.0"}]`)
	}))
	defer server.Close()
	app, stdout, _ := testApp(t, "")
	app.Info.Version = "1.0.0"
	app.upgradeReleaseAPIURL = server.URL
	app.executablePath = func() (string, error) {
		return filepath.Join(npmRoot, "@flintpay", "cli-darwin-arm64", "bin", "flint"), nil
	}
	app.runtimeGOOS = "darwin"
	app.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "npm" && slices.Equal(args, []string{"root", "-g"}) {
			return []byte(npmRoot + "\n"), nil
		}
		if len(args) > 0 && args[0] == "version" {
			return []byte("1.0.0\n"), nil
		}
		return nil, nil
	}

	if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitSoftware {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
	if !strings.Contains(stdout.String(), `"code":"UPGRADE_VERIFICATION_FAILED"`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestPackageManagerMismatchDoesNotInstall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"cli/v1.1.0"}]`)
	}))
	defer server.Close()
	app, stdout, _ := testApp(t, "")
	app.Info.Version = "1.0.0"
	app.upgradeReleaseAPIURL = server.URL
	app.executablePath = func() (string, error) {
		return "/usr/local/lib/node_modules/@flintpay/cli-darwin-arm64/bin/flint", nil
	}
	app.runtimeGOOS = "darwin"
	var commands []string
	app.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, strings.Join(append([]string{name}, args...), " "))
		return []byte("/opt/homebrew/lib/node_modules\n"), nil
	}

	if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitSoftware {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
	if !strings.Contains(stdout.String(), `"code":"PACKAGE_MANAGER_MISMATCH"`) {
		t.Fatalf("stdout=%s", stdout)
	}
	if !slices.Equal(commands, []string{"npm root -g"}) {
		t.Fatalf("commands=%v", commands)
	}
}

func TestWindowsNPMUpgradeRequiresRestartWithExactVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"cli/v1.1.0"}]`)
	}))
	defer server.Close()
	app, stdout, _ := testApp(t, "")
	app.Info.Version = "1.0.0"
	app.upgradeReleaseAPIURL = server.URL
	app.executablePath = func() (string, error) {
		return `C:\npm\node_modules\@flintpay\cli-windows-amd64\bin\flint.exe`, nil
	}
	app.runtimeGOOS = "windows"
	app.runtimeGOARCH = "amd64"
	app.runCommand = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("ran package manager while the Windows binary was open")
		return nil, nil
	}

	if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitUsage {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
	if !strings.Contains(stdout.String(), `npm install -g @flintpay/cli@1.1.0`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestWindowsStandaloneUpgradePrintsExactAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"cli/v1.1.0"}]`)
	}))
	defer server.Close()
	executable := filepath.Join(t.TempDir(), "flint.exe")
	if err := os.WriteFile(executable, []byte("old flint"), 0o755); err != nil {
		t.Fatal(err)
	}
	app, stdout, _ := testApp(t, "")
	app.Info.Version = "1.0.0"
	app.upgradeReleaseAPIURL = server.URL
	app.upgradeReleaseDownloadURL = "https://downloads.example/releases"
	app.executablePath = func() (string, error) { return executable, nil }
	app.runtimeGOOS = "windows"
	app.runtimeGOARCH = "amd64"

	if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitUsage {
		t.Fatalf("exit=%d stdout=%s", exit, stdout)
	}
	want := `https://downloads.example/releases/cli%2Fv1.1.0/flint_1.1.0_windows_amd64.zip`
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestUpgradeDoesNothingWhenCurrent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"tag_name":"cli/v2.0.0"}]`)
	}))
	defer server.Close()
	app, stdout, stderr := testApp(t, "")
	app.Info.Version = "2.0.0"
	app.upgradeReleaseAPIURL = server.URL
	app.executablePath = func() (string, error) { t.Fatal("looked up executable for current version"); return "", nil }

	if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout.String(), `"status":"current"`) {
		t.Fatalf("stdout=%s", stdout)
	}
}

func TestDetectInstallMethod(t *testing.T) {
	for _, test := range []struct{ path, want string }{
		{"/usr/local/lib/node_modules/@flintpay/cli-linux-amd64/bin/flint", "npm"},
		{"/home/linuxbrew/.linuxbrew/Cellar/flint/1.0.0/bin/flint", "homebrew"},
		{"/Users/test/.local/bin/flint", "standalone"},
	} {
		if got := detectInstallMethod(test.path); got != test.want {
			t.Errorf("detectInstallMethod(%q)=%q want %q", test.path, got, test.want)
		}
	}
}

func TestReleaseVersionValidationAndOrdering(t *testing.T) {
	for _, value := range []string{"0.1.0", "v1.2.3", "2.0.0-beta.1", "2.0.0-rc.10"} {
		if _, ok := parseReleaseVersion(value); !ok {
			t.Errorf("valid version %q was rejected", value)
		}
	}
	for _, value := range []string{"1.2", "01.2.3", "1.2.3-", "1.2.3-beta..1", "1.2.3-beta.01", "1.2.3+build"} {
		if _, ok := parseReleaseVersion(value); ok {
			t.Errorf("invalid version %q was accepted", value)
		}
	}
	for _, pair := range [][2]string{{"1.0.0-beta.2", "1.0.0-beta.10"}, {"1.0.0-1", "1.0.0-alpha"}, {"1.0.0-alpha", "1.0.0-alpha.1"}, {"1.0.0-rc.1", "1.0.0"}} {
		left, _ := parseReleaseVersion(pair[0])
		right, _ := parseReleaseVersion(pair[1])
		if compareReleaseVersions(left, right) >= 0 {
			t.Errorf("expected %s before %s", pair[0], pair[1])
		}
	}
}

func TestTailBufferKeepsBoundedSuffix(t *testing.T) {
	buffer := &tailBuffer{limit: 5}
	for _, value := range []string{"abc", "defg", "123456"} {
		if written, err := buffer.Write([]byte(value)); err != nil || written != len(value) {
			t.Fatalf("Write(%q)=(%d, %v)", value, written, err)
		}
	}
	if got := string(buffer.data); got != "23456" {
		t.Fatalf("tail=%q", got)
	}
}

func testUpgradeArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "flint", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
