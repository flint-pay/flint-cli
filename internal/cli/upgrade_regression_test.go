package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStandaloneUpgradePreservesConcurrentNewerVersion(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("standalone self-upgrade requires macOS or Linux")
	}
	binaryForVersion := func(version string) []byte { return []byte("#!/bin/sh\necho " + version + "\n") }
	archive := testUpgradeArchive(t, binaryForVersion("1.1.0"))
	digest := sha256.Sum256(archive)
	archiveName := fmt.Sprintf("flint_1.1.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/releases":
			fmt.Fprint(w, `[{"tag_name":"cli/v1.1.0"}]`)
		case strings.HasSuffix(request.URL.Path, "/checksums.txt"):
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(digest[:]), archiveName)
		case strings.HasSuffix(request.URL.Path, "/"+archiveName):
			w.Write(archive)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	executable := filepath.Join(t.TempDir(), "flint")
	if err := os.WriteFile(executable, binaryForVersion("1.0.0"), 0o700); err != nil {
		t.Fatal(err)
	}
	app, stdout, stderr := testApp(t, "")
	app.Info.Version = "1.0.0"
	app.upgradeReleaseAPIURL = server.URL + "/releases"
	app.upgradeReleaseDownloadURL = server.URL + "/download"
	app.executablePath = func() (string, error) { return executable, nil }
	app.runCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		output, err := runUpgradeCommand(ctx, name, args...)
		if err == nil && strings.HasPrefix(filepath.Base(name), ".flint-upgrade-") {
			newerApp, _, _ := testApp(t, "")
			if actual, newerErr := newerApp.replaceExecutableAtomically(ctx, executable, binaryForVersion("1.2.0"), "1.2.0"); newerErr != nil || actual != "1.2.0" {
				return nil, fmt.Errorf("newer updater failed: version=%s error=%v", actual, newerErr)
			}
		}
		return output, err
	}
	if exit := app.Run([]string{"upgrade", "--output", "json"}); exit != ExitOK {
		t.Fatalf("exit=%d output=%s stderr=%s", exit, stdout, stderr)
	}
	for _, want := range []string{`"status":"current"`, `"installed_version":"1.2.0"`, `"latest_version":"1.1.0"`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("missing %s in %s", want, stdout)
		}
	}
	if output, err := runUpgradeCommand(t.Context(), executable, "version"); err != nil || strings.TrimSpace(string(output)) != "1.2.0" {
		t.Fatalf("newer installation was overwritten: output=%q error=%v", output, err)
	}
}

func TestUpgradeDownloadCancellationWhileReadingBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reading := make(chan struct{})
	app, _, _ := testApp(t, "")
	app.HTTPClient = &http.Client{Transport: listenRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(request)
		if err == nil {
			response.Body = &upgradeReadSignal{ReadCloser: response.Body, started: reading}
		}
		return response, err
	})}
	result := make(chan *CLIError, 1)
	go func() {
		_, err := app.getUpgradeURL(ctx, server.URL, maxReleaseResponseSize)
		result <- err
	}()
	select {
	case <-reading:
	case <-time.After(5 * time.Second):
		t.Fatal("download never began reading the flushed response body")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil || err.Code != "REQUEST_CANCELED" || !errors.Is(err.Cause, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("download did not stop after cancellation")
	}
}

type upgradeReadSignal struct {
	io.ReadCloser
	started chan struct{}
	once    sync.Once
}

func (r *upgradeReadSignal) Read(data []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	return r.ReadCloser.Read(data)
}
