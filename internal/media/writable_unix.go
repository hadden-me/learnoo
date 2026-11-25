//go:build unix

package media

import (
	"fmt"
	"syscall"
)

// checkWritable asks the kernel whether dir is writable (access(2) with
// W_OK): it reflects permissions and read-only mounts without writing.
func checkWritable(dir string) error {
	const wOK = 0x2
	if err := syscall.Access(dir, wOK); err != nil {
		return fmt.Errorf("media: storage directory not writable: %w", err)
	}
	return nil
}
