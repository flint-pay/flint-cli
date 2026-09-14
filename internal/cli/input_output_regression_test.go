package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestJSONInputCancellation(t *testing.T) {
	for _, prefix := range []string{"", `{"name":"unfinished`, `{"name":"complete"}`} {
		t.Run(prefix, func(t *testing.T) {
			app, out, stderr := testApp(t, "")
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			started := make(chan struct{})
			app.Stdin = io.MultiReader(strings.NewReader(prefix), startedInput{reader, started})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			app.Context = ctx
			done := make(chan int, 1)
			go func() {
				done <- app.Run([]string{"customers", "create", "--input", "-", "--dry-run=client", "--output", "json"})
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("command did not reach blocked input read")
			}
			cancel()
			select {
			case exit := <-done:
				if exit != ExitNetwork || !strings.Contains(out.String(), "REQUEST_CANCELED") {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
				}
			case <-time.After(time.Second):
				t.Fatal("JSON input ignored cancellation")
			}
		})
	}
}

func TestResultOutputFailuresReturnNonzero(t *testing.T) {
	for _, args := range [][]string{
		{"version"},
		{"version", "--field", "data.cli_version"},
		{"version", "--output", "json"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, _, stderr := testApp(t, "")
			app.Stdout = failingWriter{}
			if exit := app.Run(args); exit != ExitNetwork {
				t.Fatalf("exit=%d stderr=%s", exit, stderr)
			}
		})
	}
}

type failAfterWriter struct {
	remaining int
	calls     int
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.calls++
	n := min(len(p), w.remaining)
	w.remaining -= n
	// Model a short write without an error as well as an initial full write.
	return n, nil
}

func TestHumanRenderersPreserveOutputFailure(t *testing.T) {
	for _, render := range []string{"", "checkout", "request_log", "help_search", "support_open"} {
		t.Run(render, func(t *testing.T) {
			app, _, _ := testApp(t, "")
			writer := &failAfterWriter{remaining: 1}
			app.Stdout = writer
			value := map[string]any{"data": map[string]any{
				"url": "https://example.com", "ask_url": "https://example.com",
				"status": "pending", "recommended_action": "retry",
			}}
			err := app.writeResult(value, &Command{Render: render}, defaultOptions())
			if err == nil || err.Code != "OUTPUT_WRITE_FAILED" || !errors.Is(err.Cause, io.ErrShortWrite) {
				t.Fatalf("expected short write error, got %v", err)
			}
			if writer.calls != 1 {
				t.Fatalf("renderer kept writing after failure: %d calls", writer.calls)
			}
		})
	}
}

func TestNullFieldOutputFailure(t *testing.T) {
	app, _, _ := testApp(t, "")
	app.Stdout = failingWriter{}
	opts := defaultOptions()
	opts.Field = "data"
	err := app.writeResult(map[string]any{"data": nil}, nil, opts)
	if err == nil || err.Code != "OUTPUT_WRITE_FAILED" {
		t.Fatalf("expected output failure for null field, got %v", err)
	}
}
