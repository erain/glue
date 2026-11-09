// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

package jobs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func acquireLock(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "LOCK")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("jobs: cannot acquire %s (another manager may own this state directory): %w", path, err)
	}
	return f, nil
}

func releaseLock(f *os.File) error {
	// Keep the inode: unlinking would let another opener lock a different file.
	return errors.Join(unlockFile(f), f.Close())
}
