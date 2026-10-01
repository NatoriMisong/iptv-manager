//go:build !linux

package resolver

import "os/exec"

// Linux is the deployment target. Other platforms retain CommandContext's
// direct-child cancellation so development builds remain portable.
func configureProcess(cmd *exec.Cmd) {}
func cleanupProcess(cmd *exec.Cmd)   {}
