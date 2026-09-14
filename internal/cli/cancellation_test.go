package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestCanceledLocalMutationsDoNotWaitOrWrite(t *testing.T) {
	for _, command := range [][]string{
		{"config", "set", "profile", "changed"},
		{"history", "--clear", "--confirm"},
		{"auth", "logout", "--confirm"},
	} {
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			a, out, stderr := testApp(t, "")
			for _, name := range []string{"FLINT_API_KEY", "FLINT_ACCESS_TOKEN", "FLINT_CHECKOUT_SESSION_SECRET"} {
				t.Setenv(name, "")
			}
			a.DeleteCredential = func(string) error { t.Error("canceled logout deleted a credential"); return nil }
			name := "config.json"
			original := []byte(`{"default_profile":"default","profiles":{}}`)
			if command[0] == "history" {
				name = "history.json"
				original = []byte(`{"entries":[{"id":"cus_keep","profile":"default","environment":"sandbox"}]}`)
			}
			path := filepath.Join(a.ConfigDir, name)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			lock := flock.New(path + ".lock")
			if err := lock.Lock(); err != nil {
				t.Fatal(err)
			}
			defer lock.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.Context = ctx
			done := make(chan int, 1)
			go func() { done <- a.Run(append(command, "--output", "json")) }()
			select {
			case exit := <-done:
				t.Fatalf("command returned before lock release: exit=%d stdout=%s stderr=%s", exit, out, stderr)
			case <-time.After(100 * time.Millisecond):
			}
			cancel()
			select {
			case exit := <-done:
				if exit != ExitNetwork || !strings.Contains(out.String(), "REQUEST_CANCELED") {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled command is still waiting for the lock")
			}
			current, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(current, original) {
				t.Fatalf("canceled mutation changed file: %s, err=%v", current, err)
			}
		})
	}
}

func TestLocalTransactionFinishesAfterStarting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := false
	err := withLocalFileLock(ctx, filepath.Join(t.TempDir(), "config.json"), func() error {
		cancel()
		// Once started, persistence and rollback must be allowed to finish.
		completed = true
		return nil
	})
	if err != nil || !completed {
		t.Fatalf("transaction interrupted: completed=%t err=%v", completed, err)
	}
	err = withLocalFileLock(ctx, filepath.Join(t.TempDir(), "config.json"), func() error {
		t.Error("transaction started with a canceled context")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

type startedInput struct {
	io.Reader
	started chan struct{}
}

func (r startedInput) Read(p []byte) (int, error) {
	close(r.started)
	return r.Reader.Read(p)
}

func TestPromptReadsHonorCancellation(t *testing.T) {
	for _, command := range [][]string{
		{"auth", "import", "--stdin"},
		{"history", "--clear"},
		{"signup"},
	} {
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			a, out, stderr := testApp(t, "")
			a.IsTTY = func() bool { return true }
			reader, writer := io.Pipe()
			defer writer.Close()
			defer reader.Close()
			started := make(chan struct{})
			a.Stdin = startedInput{reader, started}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.Context = ctx
			done := make(chan int, 1)
			go func() { done <- a.Run(command) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("command did not read input")
			}
			cancel()
			select {
			case exit := <-done:
				if exit != ExitNetwork {
					t.Fatalf("exit=%d stdout=%s stderr=%s", exit, out, stderr)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled prompt is still reading input")
			}
		})
	}
}
