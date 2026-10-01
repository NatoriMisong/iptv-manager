//go:build linux

package resolver

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// A JavaScript runtime is a descendant of yt-dlp. Give each extraction its own
// process group, so cancellation also stops Node/Deno and their worker children.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func cleanupProcess(cmd *exec.Cmd) {
	// A parent can exit while a descendant retains stdout/stderr. WaitDelay
	// bounds pipe draining, then this cleanup stops any remaining group members.
	_ = killProcessGroup(cmd)
}
