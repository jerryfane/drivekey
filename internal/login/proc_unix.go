//go:build !windows

package login

import (
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// killProcessGroup stops the helper and the gcloud process it started; the helper
// is a session leader, so its pid is also its process group id.
func killProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
}
