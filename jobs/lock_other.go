// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd && !windows

package jobs

import (
	"fmt"
	"os"
	"runtime"
)

func lockFile(*os.File) error   { return fmt.Errorf("state locking is unsupported on %s", runtime.GOOS) }
func unlockFile(*os.File) error { return nil }
