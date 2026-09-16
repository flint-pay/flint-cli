package cli

import (
	"context"
	"fmt"
	"time"
)

func (a *App) withUpgradeProgress(ctx context.Context, opts Options, stage string, action func()) {
	a.upgradeProgress(ctx, opts, stage, 10*time.Second, action)
}

// Each stage waits for its reporter to stop before returning, so progress cannot
// race with the next stage or appear after the final result or error.
func (a *App) upgradeProgress(ctx context.Context, opts Options, stage string, interval time.Duration, action func()) {
	if opts.Quiet || opts.Progress == "quiet" || (opts.Progress == "auto" && opts.Output != "human") {
		action()
		return
	}
	_ = a.writeProgress(opts, map[string]any{"type": "upgrade", "stage": stage}, stage+"…")
	stop, done := make(chan struct{}), make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				elapsed := int(time.Since(started).Seconds())
				_ = a.writeProgress(opts, map[string]any{"type": "upgrade", "stage": stage, "elapsed_seconds": elapsed}, fmt.Sprintf("Still working: %s (%ds elapsed)", stage, elapsed))
			}
		}
	}()
	defer func() { close(stop); <-done }()
	action()
}
