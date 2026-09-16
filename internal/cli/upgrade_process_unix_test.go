//go:build darwin || linux

package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpgradeTimeoutStopsDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-finished")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := runUpgradeCommand(ctx, "/bin/sh", "-c", `(sleep 2; printf done > "$1") & wait`, "probe", marker)
	if err == nil || ctx.Err() != context.DeadlineExceeded {
		t.Fatalf("expected timeout: command=%v context=%v", err, ctx.Err())
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("command waited for descendants after cancellation: %s", elapsed)
	}
	// Give a surviving child enough time to write its marker, even if the
	// command returned early by closing its output pipes.
	if remaining := 2300*time.Millisecond - time.Since(start); remaining > 0 {
		time.Sleep(remaining)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("descendant survived cancellation: %v", err)
	}
}
