//go:build !darwin && !linux

package cli

import "os/exec"

// These platforms print manual upgrade instructions and never launch an updater.
func configureUpgradeProcess(command *exec.Cmd) {}
