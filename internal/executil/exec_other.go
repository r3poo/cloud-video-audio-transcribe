//go:build !windows

// Package executil hides console windows for child processes spawned by a GUI
// application. On non-Windows platforms there is nothing to hide.
package executil

import "os/exec"

// Hide is a no-op outside Windows.
func Hide(cmd *exec.Cmd) {}
