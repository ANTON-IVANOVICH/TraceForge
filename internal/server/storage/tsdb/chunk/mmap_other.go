//go:build !unix

package chunk

import "os"

// mapData falls back to reading the whole file into memory on platforms without
// the Unix mmap syscall (e.g. Windows). The API is identical; only the backing
// mechanism differs.
func mapData(path string) ([]byte, func() error, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return data, func() error { return nil }, nil
}
