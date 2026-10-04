package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// errLocked is returned by tryLock when another process holds the lock.
var errLocked = errors.New("locked")

// Lock is an exclusive, cross-process lock on the state directory.
type Lock struct{ f *os.File }

// LockFile is the path of the state lock.
func (d Dir) LockFile() string { return filepath.Join(d.Root, "lock") }

// Lock takes the state lock, waiting up to timeout. Every command that reads and rewrites
// shared state (config, change feeds, the pending login) holds it for the whole
// read-modify-write, so concurrent drivekey processes cannot overwrite each other.
func (d Dir) Lock(ctx context.Context, timeout time.Duration) (*Lock, error) {
	if err := d.Claim(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(d.LockFile(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := tryLock(f)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, errLocked) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("another drivekey command is still running (waited %s for %s)", timeout, d.LockFile())
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Unlock releases the lock.
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = unlock(l.f)
	_ = l.f.Close()
	l.f = nil
}
