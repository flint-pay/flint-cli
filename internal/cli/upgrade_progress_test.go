package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type upgradeProgressWriter struct {
	sync.Mutex
	buffer    bytes.Buffer
	heartbeat chan struct{}
	once      sync.Once
}

func (w *upgradeProgressWriter) Write(p []byte) (int, error) {
	w.Lock()
	defer w.Unlock()
	n, e := w.buffer.Write(p)
	if strings.Contains(string(p), "Still working:") || strings.Contains(string(p), "elapsed_seconds") {
		w.once.Do(func() { close(w.heartbeat) })
	}
	return n, e
}
func (w *upgradeProgressWriter) String() string { w.Lock(); defer w.Unlock(); return w.buffer.String() }

func TestUpgradeProgressWhileWorking(t *testing.T) {
	for _, mode := range []string{"plain", "json"} {
		t.Run(mode, func(t *testing.T) {
			app, out, _ := testApp(t, "")
			writer := &upgradeProgressWriter{heartbeat: make(chan struct{})}
			app.Stderr = writer
			opts := defaultOptions()
			opts.Progress = mode
			app.upgradeProgress(t.Context(), opts, "Updating Homebrew package information", time.Millisecond, func() {
				if !strings.Contains(writer.String(), "Updating Homebrew package information") {
					t.Error("stage was not announced before the operation")
				}
				select {
				case <-writer.heartbeat:
				case <-time.After(2 * time.Second):
					t.Error("no progress while operation was blocked")
				}
			})
			if out.Len() != 0 {
				t.Fatal("progress polluted stdout")
			}
			// The helper has joined the reporter; accessing its buffer is safe now.
			if writer.buffer.Len() == 0 {
				t.Fatal("no progress was written")
			}
		})
	}
}

func TestUpgradeProgressQuietAndCanceled(t *testing.T) {
	for _, name := range []string{"quiet", "progress-quiet", "json-auto", "canceled"} {
		t.Run(name, func(t *testing.T) {
			app, _, stderr := testApp(t, "")
			opts := defaultOptions()
			switch name {
			case "quiet":
				opts.Quiet = true
			case "progress-quiet":
				opts.Progress = "quiet"
			case "json-auto":
				opts.Output = "json"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "canceled" {
				cancel()
			}
			called := false
			app.upgradeProgress(ctx, opts, "Checking releases", time.Hour, func() { called = true })
			if !called {
				t.Fatal("progress skipped the operation")
			}
			if name != "canceled" && stderr.Len() != 0 {
				t.Fatalf("unexpected output: %s", stderr)
			}
			if name == "canceled" && strings.Contains(stderr.String(), "Still working") {
				t.Fatal("heartbeat after cancellation")
			}
		})
	}
}
