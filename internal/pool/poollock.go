package pool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// pollInterval is how often TryLockContext retries while blocked on a held
// lock. Small enough that a canceled context is noticed promptly in tests.
const pollInterval = 25 * time.Millisecond

// PoolLock is the single exclusive advisory lock every mutating pool command
// (install, update, sync, rollback, gc, refresh, policy set) takes before
// reading pool state, and releases on exit (§6.5). `list` never takes it.
// Implemented with github.com/gofrs/flock: flock(2) on POSIX, LockFileEx on
// Windows — the portable dependency the pool needs; this is distinct from
// cmd/up.go's existing Linux-oriented flock use, which stays where it is.
type PoolLock struct {
	fl *flock.Flock
}

// NewPoolLock returns a PoolLock bound to the advisory lock file at path
// (normally AdvisoryLockFile()). The file and its parent directory are
// created on first Acquire if missing.
func NewPoolLock(path string) *PoolLock {
	return &PoolLock{fl: flock.New(path)}
}

// Acquire blocks, polling every pollInterval, until the exclusive lock is
// obtained or ctx is canceled/times out. A canceled context returns ctx.Err()
// wrapped, not the raw flock error, so callers can reliably check
// errors.Is(err, context.Canceled).
func (l *PoolLock) Acquire(ctx context.Context) error {
	dir := filepath.Dir(l.fl.Path())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating pool lock directory %s: %w", dir, err)
	}

	locked, err := l.fl.TryLockContext(ctx, pollInterval)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("acquiring pool lock: %w", ctxErr)
		}
		return fmt.Errorf("acquiring pool lock: %w", err)
	}
	if !locked {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("acquiring pool lock: %w", ctxErr)
		}
		return fmt.Errorf("acquiring pool lock: not locked")
	}
	return nil
}

// Release releases the lock. Safe to call even if Acquire was never called
// successfully (flock.Unlock on an unlocked Flock is a no-op).
func (l *PoolLock) Release() error {
	return l.fl.Unlock()
}
