//go:build !unix

package main

import (
	"fmt"
	"runtime"
)

// syscallExecImpl is the non-unix stub. v1 ships darwin + linux only; on
// Windows the user can use --print and paste the command into their shell,
// or run gpu under WSL. Per research §11.1.
func syscallExecImpl(host string, port int, extraArgs []string) error {
	return fmt.Errorf("`gpu instances ssh` is not supported on %s (no syscall.Exec equivalent) — use `gpu instances ssh --print <id>` to get the ssh command, or run gpu under WSL", runtime.GOOS)
}
