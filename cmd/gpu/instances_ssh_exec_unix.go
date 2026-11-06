//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// syscallExecImpl replaces the current process image with /usr/bin/ssh (or
// wherever LookPath finds it). On success it never returns — the process
// is gone. On failure (ssh not on PATH, exec failure) it returns an error.
//
// Why syscall.Exec and not exec.Cmd.Run: The TL;DR
// is that exec.Cmd keeps the gpu process alive as a parent, which breaks
// signal handling and tty allocation for ssh. syscall.Exec replaces the
// image cleanly so the user gets the same UX as running ssh directly.
func syscallExecImpl(host string, port int, extraArgs []string) error {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh not found on PATH (install OpenSSH client to use `gpu instances ssh`): %w", err)
	}
	argv := buildSSHArgv(host, port, extraArgs)
	// syscall.Exec NEVER returns on success — the gpu process image is replaced.
	return syscall.Exec(sshBin, argv, os.Environ())
}
