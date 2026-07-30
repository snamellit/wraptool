package pool

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestPoolLockAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")

	l := NewPoolLock(path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := l.Acquire(ctx); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// The lock must be re-acquirable after release.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := l.Acquire(ctx2); err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	_ = l.Release()
}

func TestPoolLockContentionBlocksUntilContextCanceled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")

	first := NewPoolLock(path)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	if err := first.Acquire(ctx1); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer first.Release()

	second := NewPoolLock(path)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()

	start := time.Now()
	err := second.Acquire(ctx2)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the second Acquire to fail while the first lock is held")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected a context.DeadlineExceeded-wrapping error, got: %v", err)
	}
	// It should have actually blocked for close to the deadline, not failed
	// immediately.
	if elapsed < 200*time.Millisecond {
		t.Errorf("second Acquire returned too quickly (%v); it should have blocked until the deadline", elapsed)
	}
}

func TestPoolLockSecondAcquireSucceedsAfterFirstReleases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")

	first := NewPoolLock(path)
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	if err := first.Acquire(ctx1); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	second := NewPoolLock(path)
	released := make(chan struct{})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = first.Release()
		close(released)
	}()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := second.Acquire(ctx2); err != nil {
		t.Fatalf("second Acquire after release: %v", err)
	}
	<-released
	_ = second.Release()
}
