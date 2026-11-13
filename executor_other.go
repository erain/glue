// Copyright (c) 2026 Yu YI. Licensed under the MIT License.

//go:build !unix

package glue

import "os/exec"

func killProcessGroupOnCancel(*exec.Cmd) {}
