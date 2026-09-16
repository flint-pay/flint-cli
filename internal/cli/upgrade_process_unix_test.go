//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpgradeCommandPreservesFormulaMetadata(t *testing.T) {
	metadata := `{"formulae":[{"desc":"` + strings.Repeat("x", 4096) + `","versions":{"stable":"0.3.0"}}]}`
	output, err := runUpgradeCommand(t.Context(), "/bin/sh", "-c", `printf '%s' "$1"`, "probe", metadata)
	if err != nil || !json.Valid(output) || string(output) != metadata {
		t.Fatalf("formula metadata was truncated: bytes=%d error=%v", len(output), err)
	}
}

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
