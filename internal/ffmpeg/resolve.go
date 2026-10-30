// Package ffmpeg locates or obtains an ffmpeg binary and drives it for probing,
// silence detection and audio extraction.
package ffmpeg

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"cloudtranscribe/internal/executil"
)

// Install describes a usable ffmpeg/ffprobe pair.
type Install struct {
	FFmpeg  string
	FFprobe string
	Version string
	// Origin explains where the binary came from, for the UI status line.
	Origin string
}

// Describe renders a one-line summary for the status area.
func (i *Install) Describe() string {
	if i == nil || i.FFmpeg == "" {
		return "ffmpeg not found"
	}
	version := i.Version
	if version == "" {
		version = "unknown version"
	}
	return fmt.Sprintf("ffmpeg %s  (%s)", version, i.Origin)
}

// HasFFprobe reports whether a usable ffprobe was found next to ffmpeg.
func (i *Install) HasFFprobe() bool {
	return i != nil && i.FFprobe != ""
}

func binaryName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// CacheDir is where a downloaded ffmpeg is kept between runs.
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "cloudtranscribe", "ffmpeg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create ffmpeg cache directory: %w", err)
	}
	return dir, nil
}

type candidate struct {
	path   string
	origin string
}

// candidates lists the places a local ffmpeg install may live, in priority
// order. The system PATH wins so an explicit user install is never shadowed by
// a downloaded copy; the well-known install directories follow because setup
// guides tell people to drop a build in C:\ffmpeg and only some of them add it
// to PATH.
func candidates(exeDir string) []candidate {
	var out []candidate

	if p := os.Getenv("FFMPEG_PATH"); p != "" {
		out = append(out, candidate{p, "FFMPEG_PATH"})
	}
	if p, err := exec.LookPath(binaryName("ffmpeg")); err == nil {
		out = append(out, candidate{p, "system PATH"})
	}
	if exeDir != "" {
		out = append(out,
			candidate{filepath.Join(exeDir, binaryName("ffmpeg")), "next to the program"},
			candidate{filepath.Join(exeDir, "bin", binaryName("ffmpeg")), "next to the program"},
			candidate{filepath.Join(exeDir, "ffmpeg", "bin", binaryName("ffmpeg")), "next to the program"},
		)
	}
	if dir, err := CacheDir(); err == nil {
		out = append(out, candidate{filepath.Join(dir, binaryName("ffmpeg")), "local cache"})
	}
	for _, dir := range installDirs() {
		out = append(out, candidate{filepath.Join(dir, binaryName("ffmpeg")), "system install"})
	}
	return out
}

// installDirs lists the directories package managers and manual installs use
// on a typical Windows machine. The Unix paths cost nothing and make the
// automatic search work during development on WSL or in CI.
func installDirs() []string {
	dirs := []string{`C:\ffmpeg\bin`}
	if runtime.GOOS == "windows" {
		if root := os.Getenv("ProgramFiles"); root != "" {
			dirs = append(dirs, filepath.Join(root, "ffmpeg", "bin"))
		}
		if root := os.Getenv("ProgramFiles(x86)"); root != "" {
			dirs = append(dirs, filepath.Join(root, "ffmpeg", "bin"))
		}
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			dirs = append(dirs,
				filepath.Join(local, "Microsoft", "WinGet", "Links"),
				filepath.Join(local, "Programs", "ffmpeg", "bin"),
			)
		}
		if home, err := os.UserHomeDir(); err == nil {
			dirs = append(dirs, filepath.Join(home, "scoop", "shims"))
		}
		dirs = append(dirs, `C:\ProgramData\chocolatey\bin`)
	}
	return append(dirs,
		"/usr/local/bin",
		"/usr/bin",
		"/opt/homebrew/bin",
		"/snap/bin",
	)
}

// Resolve returns the first local ffmpeg that actually runs.
func Resolve(ctx context.Context, exeDir string) (*Install, bool) {
	seen := make(map[string]bool)
	for _, c := range candidates(exeDir) {
		clean := filepath.Clean(c.path)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if st, err := os.Stat(clean); err != nil || st.IsDir() {
			continue
		}
		version, ok := checkVersion(ctx, clean)
		if !ok {
			continue
		}
		return &Install{
			FFmpeg:  clean,
			FFprobe: findFFprobe(clean),
			Version: version,
			Origin:  c.origin,
		}, true
	}
	return nil, false
}

func findFFprobe(ffmpegPath string) string {
	dir := filepath.Dir(ffmpegPath)
	local := filepath.Join(dir, binaryName("ffprobe"))
	if st, err := os.Stat(local); err == nil && !st.IsDir() {
		return local
	}
	if p, err := exec.LookPath(binaryName("ffprobe")); err == nil {
		return p
	}
	return ""
}

// checkVersion runs "ffmpeg -version" and extracts the version token.
func checkVersion(ctx context.Context, path string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "-version")
	executil.Hide(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", false
	}
	v := ParseVersion(stdout.String())
	if v == "" {
		v = ParseVersion(stderr.String())
	}
	if v == "" {
		return "", false
	}
	return v, true
}

// ParseVersion pulls the version token out of the first line of
// "ffmpeg -version" output. It returns "" when no version token is present.
func ParseVersion(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "version" && i+1 < len(fields) {
				return fields[i+1]
			}
		}
		if strings.TrimSpace(line) != "" {
			return ""
		}
	}
	return ""
}

// Progress reports the state of an automatic download.
type Progress struct {
	Source     string
	Stage      string // "requesting", "downloading", "extracting", "verifying"
	Downloaded int64
	Total      int64
	// Speed is the average throughput in bytes per second while downloading.
	Speed int64
}

// ProgressFunc receives download progress updates.
type ProgressFunc func(Progress)

// Ensure returns a local ffmpeg when one exists and downloads one otherwise.
// A local install is always preferred; the download is the fallback.
func Ensure(ctx context.Context, exeDir string, prog ProgressFunc) (*Install, error) {
	if install, ok := Resolve(ctx, exeDir); ok {
		return install, nil
	}

	if runtime.GOOS != "windows" {
		return nil, fmt.Errorf("ffmpeg was not found. Install it with your package manager and try again " +
			"(automatic download is only implemented for Windows builds)")
	}

	dir, err := CacheDir()
	if err != nil {
		return nil, err
	}
	return Download(ctx, dir, prog)
}
