//go:build !windows

package browser

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

func configureDiagnosticProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func stopDiagnosticProcess(_ context.Context, process *os.Process) {
	_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
	_ = process.Kill()
}

func retryDiagnosticCleanup(error) bool { return false }
