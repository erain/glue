// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

//go:build unix

package glue

import (
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel starts c in its own process group and kills the
// whole group when its context is canceled, so a shell's children cannot keep
// the command (and its output pipes) alive.
func killProcessGroupOnCancel(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}
