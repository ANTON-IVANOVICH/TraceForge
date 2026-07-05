//go:build unix

package chunk

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// mapData memory-maps the file read-only. The returned closeFn unmaps and closes.
func mapData(path string) ([]byte, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	size := int(stat.Size())
	if size == 0 {
		_ = f.Close()
		return nil, nil, fmt.Errorf("empty chunk data file %s", path)
	}
	data, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("mmap %s: %w", path, err)
	}
	closeFn := func() error {
		merr := unix.Munmap(data)
		cerr := f.Close()
		if merr != nil {
			return merr
		}
		return cerr
	}
	return data, closeFn, nil
}
