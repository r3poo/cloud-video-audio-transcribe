//go:build windows

// Package executil hides console windows for child processes spawned by a GUI
// application.
package executil

import (
	"os/exec"
	"syscall"
)

// createNoWindow keeps ffmpeg from flashing a console window when the GUI app
// launches it.
const createNoWindow = 0x08000000

// Hide prevents the child process from opening a console window.
func Hide(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
