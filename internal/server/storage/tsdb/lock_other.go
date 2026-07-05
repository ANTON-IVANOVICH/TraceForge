//go:build !unix

package tsdb

import (
	"fmt"
	"os"
	"path/filepath"
)

// acquireLock falls back to an O_EXCL lock file on platforms without flock. It
// is best-effort: a hard crash can leave a stale LOCK file that must be removed
// by hand.
func acquireLock(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("tsdb %s appears locked (LOCK file exists): %w", dir, err)
	}
	return f, nil
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	name := f.Name()
	err := f.Close()
	_ = os.Remove(name)
	return err
}
