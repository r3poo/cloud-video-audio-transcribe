package ffmpeg

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Source is one place an ffmpeg Windows build can be fetched from.
type Source struct {
	Name string
	URL  string
}

// Sources are tried in order. gyan.dev is the build host linked from
// ffmpeg.org; the BtbN GitHub release is the backup.
func Sources() []Source {
	return []Source{
		{Name: "gyan.dev", URL: "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"},
		{Name: "BtbN/FFmpeg-Builds", URL: "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip"},
	}
}

// Download fetches the first source that works, extracts ffmpeg.exe (and
// ffprobe.exe when present) into destDir and verifies that it runs.
//
// The archive itself is held in a temporary file because zip readers need
// random access; it is deleted before Download returns. Media being
// transcribed is never written to disk.
func Download(ctx context.Context, destDir string, prog ProgressFunc) (*Install, error) {
	return downloadFirst(ctx, Sources(), destDir, prog)
}

// downloadFirst is Download with an explicit source list, so the failover path
// can be exercised without reaching the internet.
func downloadFirst(ctx context.Context, sources []Source, destDir string, prog ProgressFunc) (*Install, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", destDir, err)
	}

	var problems []string
	for _, src := range sources {
		install, err := downloadFrom(ctx, src, destDir, prog)
		if err == nil {
			return install, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		problems = append(problems, fmt.Sprintf("%s: %v", src.Name, err))
	}

	return nil, fmt.Errorf("could not obtain ffmpeg automatically:\n  %s", strings.Join(problems, "\n  "))
}

func downloadFrom(ctx context.Context, src Source, destDir string, prog ProgressFunc) (*Install, error) {
	report(prog, Progress{Source: src.Name, Stage: "requesting"})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cloudtranscribe/0.1 (+ffmpeg bootstrap)")

	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	report(prog, Progress{Source: src.Name, Stage: "downloading", Total: resp.ContentLength})

	tmp, err := os.CreateTemp(destDir, "ffmpeg-download-*.zip")
	if err != nil {
		return nil, fmt.Errorf("create temporary download file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	pw := &progressWriter{w: tmp, prog: prog, source: src.Name, total: resp.ContentLength}
	if _, err := io.Copy(pw, resp.Body); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("download interrupted: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	info, err := os.Stat(tmpName)
	if err != nil {
		return nil, err
	}

	archive, err := os.Open(tmpName)
	if err != nil {
		return nil, fmt.Errorf("re-open download: %w", err)
	}
	defer archive.Close()

	reader, err := zip.NewReader(archive, info.Size())
	if err != nil {
		return nil, fmt.Errorf("downloaded file is not a valid zip: %w", err)
	}

	report(prog, Progress{Source: src.Name, Stage: "extracting"})

	ffmpegPath, ffprobePath, err := extractBinaries(reader, destDir)
	if err != nil {
		return nil, err
	}

	report(prog, Progress{Source: src.Name, Stage: "verifying"})

	version, ok := checkVersion(ctx, ffmpegPath)
	if !ok {
		os.Remove(ffmpegPath)
		return nil, fmt.Errorf("extracted ffmpeg.exe did not run")
	}

	return &Install{
		FFmpeg:  ffmpegPath,
		FFprobe: ffprobePath,
		Version: version,
		Origin:  "downloaded from " + src.Name,
	}, nil
}

// extractBinaries walks the archive and writes ffmpeg.exe and ffprobe.exe into
// destDir, replacing any previous copy.
func extractBinaries(reader *zip.Reader, destDir string) (ffmpegPath, ffprobePath string, err error) {
	want := map[string]string{
		"ffmpeg.exe":  filepath.Join(destDir, "ffmpeg.exe"),
		"ffprobe.exe": filepath.Join(destDir, "ffprobe.exe"),
	}

	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		base := strings.ToLower(path.Base(strings.ReplaceAll(file.Name, `\`, "/")))
		dest, ok := want[base]
		if !ok {
			continue
		}
		if base == "ffmpeg.exe" && ffmpegPath == "" {
			if err := extractFile(file, dest); err != nil {
				return "", "", fmt.Errorf("extract %s: %w", base, err)
			}
			ffmpegPath = dest
		} else if base == "ffprobe.exe" && ffprobePath == "" {
			if err := extractFile(file, dest); err != nil {
				// ffprobe is optional; the program falls back to parsing
				// ffmpeg's own output.
				ffprobePath = ""
				continue
			}
			ffprobePath = dest
		}
	}

	if ffmpegPath == "" {
		return "", "", fmt.Errorf("archive did not contain ffmpeg.exe")
	}
	if ffprobePath == "" {
		// A local ffprobe elsewhere is still usable.
		ffprobePath = findFFprobe(ffmpegPath)
	}
	return ffmpegPath, ffprobePath, nil
}

func extractFile(file *zip.File, dest string) error {
	rc, err := file.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		// A stale binary may be locked by another process; surface that below.
		_ = err
	}

	part := dest + ".part"
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(part)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(part)
		return err
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return err
	}
	// The zip header carries the unix mode; a running interpreter needs the bit
	// on Linux. Windows ignores it.
	if err := os.Chmod(dest, 0o755); err != nil && !os.IsNotExist(err) {
		_ = err
	}
	return nil
}

func report(prog ProgressFunc, p Progress) {
	if prog != nil {
		prog(p)
	}
}

// progressWriter throttles progress callbacks so the UI is not flooded during a
// ~115 MB download.
type progressWriter struct {
	w      io.Writer
	prog   ProgressFunc
	source string
	total  int64
	done   int64
	last   time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.prog != nil && time.Since(p.last) > 200*time.Millisecond {
		p.last = time.Now()
		p.prog(Progress{Source: p.source, Stage: "downloading", Downloaded: p.done, Total: p.total})
	}
	return n, err
}
