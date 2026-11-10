//go:build !windows

package browser

import (
	"os"
	"os/exec"
	"syscall"
)

func configureDiagnosticProcess(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
func stopDiagnosticProcess(process *os.Process) {
	_ = syscall.Kill(-process.Pid, syscall.SIGKILL)
	_ = process.Kill()
}
