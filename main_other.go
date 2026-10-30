//go:build !windows

package main

import (
	"fmt"
	"os"
)

// The program is Windows-first: the GUI is built on the Win32 API, so other
// platforms only get the command-line check below.
func main() {
	fmt.Fprintln(os.Stderr, "cloud-transcribe is a Windows program (Win32 GUI). Build it on Windows with:")
	fmt.Fprintln(os.Stderr, `  go build -ldflags="-H=windowsgui" -o cloud-transcribe.exe .`)
	os.Exit(1)
}
