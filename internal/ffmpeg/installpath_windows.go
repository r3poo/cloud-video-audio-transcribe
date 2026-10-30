//go:build windows

package ffmpeg

import "golang.org/x/sys/windows/registry"

// registryInstallDirs returns the install directories recorded in the registry
// by VLC. VLC itself resolves its own location this way (InstallDir under its
// Software keys, for both the current user and the machine), so an ffmpeg.exe
// parked next to VLC is found by the same lookup.
func registryInstallDirs() []string {
	var dirs []string
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		for _, path := range []string{
			`Software\VideoLAN\VLC`,
			`Software\WOW6432Node\VideoLAN\VLC`,
		} {
			key, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			value, _, err := key.GetStringValue("InstallDir")
			key.Close()
			if err == nil && value != "" {
				dirs = append(dirs, value)
			}
		}
	}
	return dirs
}
