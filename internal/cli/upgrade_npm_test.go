package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func makeNPMTestWrapper(t *testing.T, wrapper, goos, goarch string) {
	t.Helper()
	platform := filepath.Join(wrapper, "node_modules", "@flintpay", "cli-"+goos+"-"+goarch, "bin", "flint")
	if err := os.MkdirAll(filepath.Dir(platform), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(platform, []byte("platform binary"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestNPMUpgradePreservesPackageOnFailure(t *testing.T) {
	for _, stage := range []string{"download", "missing-platform", "staged-version", "installed-version", "staged-canceled", "installed-canceled"} {
		t.Run(stage, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			root := filepath.Join(t.TempDir(), "node_modules")
			wrapper := filepath.Join(root, "@flintpay", "cli")
			makeNPMTestWrapper(t, wrapper, app.runtimeGOOS, app.runtimeGOARCH)
			marker := filepath.Join(wrapper, "previous")
			if err := os.WriteFile(marker, []byte("working installation"), 0o644); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var prefix string
			app.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "npm" && args[0] == "config" {
					return []byte(filepath.Join(root, "npmrc")), nil
				}
				if name == "npm" {
					prefix = args[3]
					if stage == "download" {
						return nil, errors.New("download failed")
					}
					if stage != "missing-platform" {
						makeNPMTestWrapper(t, filepath.Join(prefix, "lib", "node_modules", "@flintpay", "cli"), app.runtimeGOOS, app.runtimeGOARCH)
					}
					return nil, nil
				}
				isStaged := strings.HasPrefix(args[0], prefix+string(filepath.Separator))
				if isStaged && stage == "staged-canceled" || !isStaged && stage == "installed-canceled" {
					cancel()
					return nil, ctx.Err()
				}
				if isStaged && stage == "staged-version" || !isStaged && stage == "installed-version" {
					return []byte("1.0.0\n"), nil
				}
				return []byte("1.1.0\n"), nil
			}
			_, upgradeErr := app.upgradeWithNPM(ctx, defaultOptions(), "1.1.0", root)
			if upgradeErr == nil {
				t.Fatal("upgrade unexpectedly succeeded")
			}
			wantCode := "UPGRADE_VERIFICATION_FAILED"
			if strings.HasSuffix(stage, "canceled") {
				wantCode = "REQUEST_CANCELED"
			} else if stage == "download" {
				wantCode = "PACKAGE_MANAGER_FAILED"
			}
			if upgradeErr.Code != wantCode {
				t.Fatalf("code=%s want %s", upgradeErr.Code, wantCode)
			}
			if content, err := os.ReadFile(marker); err != nil || string(content) != "working installation" {
				t.Fatalf("previous package lost: content=%q error=%v", content, err)
			}
			leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(wrapper), ".flint-npm-upgrade-*"))
			if err != nil || len(leftovers) != 0 {
				t.Fatalf("staging files remain: %v, %v", leftovers, err)
			}
		})
	}
}

func TestNPMUpgradeWithOfflinePackages(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("npm self-upgrade requires macOS or Linux")
	}
	for _, name := range []string{"npm", "node"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is unavailable", name)
		}
	}
	for _, tc := range []struct {
		name                          string
		missingPlatform, newerUpgrade bool
	}{
		{"success", false, false},
		{"missing-platform", true, false},
		{"newer-upgrade", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			app, stdout, stderr := testApp(t, "")
			platformName := "@flintpay/cli-" + app.runtimeGOOS + "-" + app.runtimeGOARCH
			wrapperSource, err := os.ReadFile(filepath.Join("..", "..", "npm", "bin", "flint.js"))
			if err != nil {
				t.Fatal(err)
			}
			makePlatform := func(version string) string {
				return npmTestArchive(t, directory, "platform-"+version, map[string]any{"name": platformName, "version": version}, "bin/flint", []byte("#!/bin/sh\necho "+version+"\n"))
			}
			makeWrapper := func(version, dependency string) string {
				return npmTestArchive(t, directory, "wrapper-"+version, map[string]any{
					"name": "@flintpay/cli", "version": version, "bin": map[string]string{"flint": "bin/flint.js"},
					"optionalDependencies": map[string]string{platformName: dependency},
				}, "bin/flint.js", wrapperSource)
			}
			previous := makeWrapper("1.0.0", "file:"+makePlatform("1.0.0"))
			dependency := "1.1.0"
			if !tc.missingPlatform {
				dependency = "file:" + makePlatform("1.1.0")
			}
			next := makeWrapper("1.1.0", dependency)
			prefix := filepath.Join(directory, "installed")
			globalConfig := filepath.Join(prefix, "etc", "npmrc")
			if err := os.MkdirAll(filepath.Dir(globalConfig), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(globalConfig, []byte("registry=http://127.0.0.1:9/\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "user.npmrc"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			isolatedNPM := func(ctx context.Context, args ...string) ([]byte, error) {
				hasConfig := false
				for _, value := range args {
					if value == "--globalconfig" {
						hasConfig = true
					}
				}
				if !hasConfig {
					args = append(args, "--globalconfig", globalConfig)
				}
				args = append(args, "--offline", "--cache", filepath.Join(directory, "cache"), "--userconfig", filepath.Join(directory, "user.npmrc"), "--ignore-scripts", "--loglevel", "error")
				return runUpgradeCommand(ctx, "npm", args...)
			}
			if _, err := isolatedNPM(t.Context(), "install", "-g", "--prefix", prefix, previous, "--include=optional", "--no-audit", "--no-fund"); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(prefix, "lib", "node_modules")
			wrapper := filepath.Join(root, "@flintpay", "cli")
			binary := filepath.Join(wrapper, "node_modules", "@flintpay", strings.TrimPrefix(platformName, "@flintpay/"), "bin", "flint")
			binLink := filepath.Join(prefix, "bin", "flint")
			assertVersion := func(want string) {
				output, err := runUpgradeCommand(t.Context(), binLink, "version", "--field", "data.cli_version", "--color", "never")
				if err != nil || strings.TrimSpace(string(output)) != want {
					t.Fatalf("installed command version=%q error=%v want=%s", output, err, want)
				}
			}
			assertVersion("1.0.0")
			var npmCompleted bool
			app.runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				if name == "npm" {
					if args[0] == "root" {
						return isolatedNPM(ctx, append(args, "--prefix", prefix)...)
					}
					if args[0] == "install" {
						preservedConfig := false
						for i, value := range args {
							if value == "--globalconfig" && i+1 < len(args) && args[i+1] == globalConfig {
								preservedConfig = true
							}
						}
						if !preservedConfig {
							return nil, errors.New("staging lost the owning npm global config")
						}
					}
					for i, value := range args {
						if value == "@flintpay/cli@1.1.0" {
							args[i] = next
						}
					}
					output, err := isolatedNPM(ctx, args...)
					if args[0] == "install" {
						npmCompleted = err == nil
						if err == nil && tc.newerUpgrade {
							newer := makeWrapper("1.2.0", "file:"+makePlatform("1.2.0"))
							newerApp, _, _ := testApp(t, "")
							newerApp.runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
								if name == "npm" {
									for i, value := range args {
										if value == "@flintpay/cli@1.2.0" {
											args[i] = newer
										}
									}
									return isolatedNPM(ctx, args...)
								}
								return runUpgradeCommand(ctx, name, args...)
							}
							if actual, newerErr := newerApp.upgradeWithNPM(ctx, defaultOptions(), "1.2.0", root); newerErr != nil || actual != "1.2.0" {
								return nil, fmt.Errorf("newer updater failed: version=%s error=%v", actual, newerErr)
							}
						}
					}
					return output, err
				}
				return runUpgradeCommand(ctx, name, args...)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `[{"tag_name":"cli/v1.1.0"}]`)
			}))
			defer server.Close()
			app.Info.Version = "1.0.0"
			app.upgradeReleaseAPIURL = server.URL
			app.executablePath = func() (string, error) { return binary, nil }
			exit := app.Run([]string{"upgrade", "--output", "json"})
			if !npmCompleted {
				t.Fatalf("npm did not exit successfully: stdout=%s stderr=%s", stdout, stderr)
			}
			if tc.missingPlatform {
				if exit != ExitSoftware || !strings.Contains(stdout.String(), `"code":"UPGRADE_VERIFICATION_FAILED"`) {
					t.Fatalf("exit=%d output=%s stderr=%s", exit, stdout, stderr)
				}
				assertVersion("1.0.0")
			} else {
				if exit != ExitOK {
					t.Fatalf("exit=%d output=%s stderr=%s", exit, stdout, stderr)
				}
				version := "1.1.0"
				status := "upgraded"
				if tc.newerUpgrade {
					version, status = "1.2.0", "current"
				}
				assertVersion(version)
				for _, want := range []string{`"installed_version":"` + version + `"`, `"status":"` + status + `"`} {
					if !strings.Contains(stdout.String(), want) {
						t.Fatalf("missing %s in %s", want, stdout)
					}
				}
			}
		})
	}
}

func TestNPMConcurrentUpgradeDoesNotRollbackAnotherUpgrade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node_modules")
	wrapper := filepath.Join(root, "@flintpay", "cli")
	first, _, _ := testApp(t, "")
	second, _, _ := testApp(t, "")
	makeNPMTestWrapper(t, wrapper, first.runtimeGOOS, first.runtimeGOARCH)
	firstVerifying := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStaged := make(chan struct{})
	secondInstalled := make(chan struct{})
	firstResult, secondResult := make(chan *CLIError, 1), make(chan *CLIError, 1)
	configure := func(app *App, marker string, firstUpgrade bool) {
		app.runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name == "npm" && args[0] == "config" {
				return []byte(filepath.Join(root, "npmrc")), nil
			}
			if name == "npm" {
				staged := filepath.Join(args[3], "lib", "node_modules", "@flintpay", "cli")
				makeNPMTestWrapper(t, staged, app.runtimeGOOS, app.runtimeGOARCH)
				return nil, os.WriteFile(filepath.Join(staged, "owner"), []byte(marker), 0o644)
			}
			if args[0] == filepath.Join(wrapper, "bin", "flint.js") {
				if firstUpgrade {
					close(firstVerifying)
					<-releaseFirst
					return nil, errors.New("first installation verification failed")
				}
				close(secondInstalled)
			} else if !firstUpgrade {
				close(secondStaged)
			}
			return []byte("1.1.0\n"), nil
		}
	}
	configure(first, "first", true)
	configure(second, "second", false)
	go func() {
		_, err := first.upgradeWithNPM(t.Context(), defaultOptions(), "1.1.0", root)
		firstResult <- err
	}()
	select {
	case <-firstVerifying:
	case <-time.After(5 * time.Second):
		t.Fatal("first upgrade never reached live verification")
	}
	go func() {
		_, err := second.upgradeWithNPM(t.Context(), defaultOptions(), "1.1.0", root)
		secondResult <- err
	}()
	select {
	case <-secondStaged:
	case <-time.After(5 * time.Second):
		close(releaseFirst)
		t.Fatal("second upgrade never completed staging")
	}
	// The first transaction still owns the live package until its rollback
	// completes. A second upgrade may download, but must not promote yet.
	select {
	case <-secondInstalled:
		close(releaseFirst)
		t.Fatal("second package replaced an in-progress transaction")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case err := <-firstResult:
		if err == nil || err.Code != "UPGRADE_VERIFICATION_FAILED" {
			t.Fatalf("first result=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first transaction did not finish")
	}
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("second result=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second transaction did not finish")
	}
	if owner, err := os.ReadFile(filepath.Join(wrapper, "owner")); err != nil || string(owner) != "second" {
		t.Fatalf("successful second upgrade was lost: owner=%q error=%v", owner, err)
	}
}

func npmTestArchive(t *testing.T, directory, name string, manifest map[string]any, executable string, source []byte) string {
	t.Helper()
	metadata, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	archive := tar.NewWriter(gz)
	for _, file := range []struct {
		name string
		data []byte
		mode int64
	}{{"package.json", metadata, 0o644}, {executable, source, 0o755}} {
		if err := archive.WriteHeader(&tar.Header{Name: "package/" + file.name, Mode: file.mode, Size: int64(len(file.data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, name+".tgz")
	if err := os.WriteFile(path, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
