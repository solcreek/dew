//go:build unix

// Package disklock provides a host-side advisory lock that stops two dew
// VMs from attaching the same disk image at once.
//
// Apple Virtualization.framework already refuses a second read-write
// attachment of a disk image that another running VM holds — but it
// surfaces that as VZErrorDomain Code=2 "storage device attachment is
// invalid", which is indistinguishable from a stale/corrupt image created
// by an older VZ. dew's old behavior was to blame the latter and tell the
// user to `rm` the image, which would destroy the *first* VM's data.
//
// Taking this lock before boot lets dew tell the two cases apart: a held
// lock means a live dew VM owns the disk (fail fast, suggest --name/--disk),
// while a clean acquire followed by a VZ Code=2 means the image really is
// stale. The lock is keyed to a "<disk>.lock" sidecar rather than the image
// itself, so it never interferes with how VZ opens the image, and the
// kernel drops it automatically when the process exits — a crashed dew
// never strands the disk.
package disklock

import (
	"errors"
	"os"
	"syscall"
)

// ErrInUse is returned by Acquire when another process already holds the
// lock for the disk image.
var ErrInUse = errors.New("disk image in use by another dew VM")

// Lock is a held advisory lock on a disk image.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive, non-blocking advisory lock keyed to
// diskPath. Returns ErrInUse if another holder already exists. Any other
// error (e.g. an unwritable directory) is returned as-is so the caller can
// decide whether to proceed unlocked.
func Acquire(diskPath string) (*Lock, error) {
	f, err := os.OpenFile(diskPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		// A held lock surfaces as EWOULDBLOCK on darwin; POSIX also permits
		// EAGAIN (the two are the same value on Linux but distinct on some
		// Unixes), so treat either as "in use" rather than a generic error.
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrInUse
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release frees the lock. Safe to call on a nil *Lock and idempotent, so
// it composes cleanly with defer.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	// Closing the fd releases the flock; the explicit unlock is belt-and-
	// suspenders for long-lived processes that reopen the same path.
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	err := l.f.Close()
	l.f = nil
	return err
}
