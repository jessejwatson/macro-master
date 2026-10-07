package store

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLocked is returned when a lock is held by someone else.
var ErrLocked = errors.New("locked")

// Lock takes an exclusive lock file at path, waiting up to wait for another
// holder to finish. A lock file older than stale is assumed abandoned and
// removed. The returned func releases the lock.
func Lock(path string, wait, stale time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, serr := os.Stat(path); serr == nil && time.Since(fi.ModTime()) > stale {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, ErrLocked
		}
		time.Sleep(50 * time.Millisecond)
	}
}
