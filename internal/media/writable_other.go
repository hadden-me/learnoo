//go:build !unix

package media

import (
	"fmt"
	"os"
)

// checkWritable falls back to the directory's permission bits where
// access(2) is unavailable.
func checkWritable(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("media: storage directory: %w", err)
	}
	if fi.Mode().Perm()&0o200 == 0 {
		return fmt.Errorf("media: storage directory %s is not writable", dir)
	}
	return nil
}
