package ffmpeg

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The program must always use an ffmpeg that is already on the machine instead
// of downloading one. FFMPEG_PATH is the most direct way to prove it.
func TestEnsurePrefersALocalInstallOverDownloading(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in binary is a shell script")
	}

	dir := t.TempDir()
	local := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(local, []byte(fakeBinary("7.1-local")), 0o755); err != nil {
		t.Fatalf("write stand-in ffmpeg: %v", err)
	}
	t.Setenv("FFMPEG_PATH", local)

	install, err := Ensure(context.Background(), dir, func(p Progress) {
		t.Errorf("Ensure started a download (%+v) although a local ffmpeg was configured", p)
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if install.FFmpeg != local {
		t.Fatalf("ffmpeg = %q, want the local binary %q", install.FFmpeg, local)
	}
	if install.Version != "7.1-local" {
		t.Errorf("version = %q, want the version the local binary printed", install.Version)
	}
	if install.Origin == "" || install.Origin == "downloaded" {
		t.Errorf("origin = %q, want it to credit the local install", install.Origin)
	}
}
