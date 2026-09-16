//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureUpgradeProcess(command *exec.Cmd) {
	// Package managers spawn helpers. Give the command its own process group
	// so cancellation stops those helpers as well as the immediate process.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}
