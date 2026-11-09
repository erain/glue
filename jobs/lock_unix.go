// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package jobs

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockFile(f *os.File) error   { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func unlockFile(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_UN) }
