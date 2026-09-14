package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

const localFileLockTimeout = 10 * time.Second

func withLocalFileLock(parent context.Context, path string, action func() error) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock := flock.New(path+".lock", flock.SetPermissions(0600))
	ctx, cancel := context.WithTimeout(parent, localFileLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	if !locked {
		return fmt.Errorf("lock %s: timed out", path)
	}
	// A lock becoming available can race cancellation. Once the transaction
	// starts, let it finish (including any rollback) before releasing the lock.
	actionErr := ctx.Err()
	if actionErr == nil {
		actionErr = action()
	}
	unlockErr := lock.Unlock()
	if actionErr != nil {
		return actionErr
	}
	if unlockErr != nil {
		return fmt.Errorf("unlock %s: %w", path, unlockErr)
	}
	return nil
}
