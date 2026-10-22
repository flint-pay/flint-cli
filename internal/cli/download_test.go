package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pdfDownloadServer(t *testing.T, pdf []byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/developer/auth-context" {
			fmt.Fprint(w, authContextJSON("sandbox"))
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write(pdf)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSaveToDashWritesRawFileToStdout(t *testing.T) {
	pdf := []byte("%PDF-1.7\n\x00\xff raw bytes\n%%EOF")
	app, stdout, stderr := testApp(t, pdfDownloadServer(t, pdf).URL)
	for _, output := range []string{"human", "json"} {
		stdout.Reset()
		if exit := app.Run([]string{"invoices", "pdf", "get", "inv_test", "--save-to", "-", "--output", output}); exit != ExitOK || stderr.Len() != 0 {
			t.Fatalf("%s exit=%d stderr=%s", output, exit, stderr)
		}
		if !bytes.Equal(stdout.Bytes(), pdf) {
			t.Fatalf("%s stdout = %q, want the raw file", output, stdout.Bytes())
		}
	}
	if _, err := os.Stat("-"); !os.IsNotExist(err) {
		t.Fatalf("--save-to - created a file named -: %v", err)
	}
}

func TestSaveToDashIsRejectedInsideMCP(t *testing.T) {
	stdout := &bytes.Buffer{}
	app := &App{Stdout: stdout, Context: context.WithValue(context.Background(), mcpSessionKey{}, newMCPSession(nil))}
	_, err := app.saveDownload(fileResponse([]byte("%PDF"), "application/pdf"), "-")
	if err == nil || err.Code != "INVALID_DESTINATION" || err.ExitCode != ExitUsage || stdout.Len() != 0 {
		t.Fatalf("error = %#v, stdout = %q", err, stdout)
	}
}

func TestSaveToReportsSpecificDestinationProblems(t *testing.T) {
	pdf := []byte("%PDF-1.7\n%%EOF")
	app, stdout, _ := testApp(t, pdfDownloadServer(t, pdf).URL)
	dir := t.TempDir()
	existing := filepath.Join(dir, "invoice.pdf")
	if err := os.WriteFile(existing, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		destination string
		code        string
		message     string
	}{
		{name: "existing file", destination: existing, code: "DESTINATION_EXISTS", message: "already exists, and downloads never overwrite files"},
		{name: "directory", destination: dir, code: "DESTINATION_IS_DIRECTORY", message: filepath.Join(dir, "download.pdf")},
		{name: "missing directory", destination: filepath.Join(dir, "missing", "invoice.pdf"), code: "DESTINATION_DIRECTORY_NOT_FOUND", message: filepath.Join(dir, "missing") + " does not exist"},
		{name: "file as directory", destination: filepath.Join(existing, "invoice.pdf"), code: "DESTINATION_DIRECTORY_NOT_FOUND", message: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout.Reset()
			exit := app.Run([]string{"invoices", "pdf", "get", "inv_test", "--save-to", test.destination, "--output", "json"})
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Param   string `json:"param"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
				t.Fatalf("output %q: %v", stdout, err)
			}
			if exit != ExitUsage || envelope.Error.Code != test.code || envelope.Error.Param != "save_to" || !strings.Contains(envelope.Error.Message, test.message) {
				t.Fatalf("exit=%d output=%s", exit, stdout)
			}
		})
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "keep me" {
		t.Fatalf("existing file changed: %q, %v", data, err)
	}
}

func TestDownloadDestinationErrorClassifiesDirectoryAcrossOpenErrors(t *testing.T) {
	dir := t.TempDir()
	for _, cause := range []error{fs.ErrExist, fs.ErrPermission, errors.New("is a directory")} {
		err := downloadDestinationError(dir, "application/pdf", &fs.PathError{Op: "open", Path: dir, Err: cause})
		if err.Code != "DESTINATION_IS_DIRECTORY" || !strings.Contains(err.Message, filepath.Join(dir, "download.pdf")) {
			t.Fatalf("cause=%v error=%v", cause, err)
		}
	}
}

func TestDownloadDestinationErrorPreservesPermissionError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "download.pdf")
	err := downloadDestinationError(path, "application/pdf", &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission})
	if err.Code != "DESTINATION_NOT_WRITABLE" {
		t.Fatalf("error=%v", err)
	}
}
