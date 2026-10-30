//go:build !windows

package ffmpeg

// registryInstallDirs reports no registry locations outside Windows. The
// search itself is still useful there because the plain install directories
// and PATH are checked.
func registryInstallDirs() []string { return nil }
