package ffmpeg

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
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
	// URL is used when the address is stable between releases.
	URL string
	// Resolve looks the address up when the file name changes per release.
	Resolve func(ctx context.Context) (string, error)
}

func (s Source) address(ctx context.Context) (string, error) {
	if s.Resolve != nil {
		return s.Resolve(ctx)
	}
	return s.URL, nil
}

const (
	// githubReleaseGyan mirrors the builds gyan.dev publishes. The bytes are the
	// same, but they are served from GitHub's CDN, which measured roughly two
	// orders of magnitude faster than www.gyan.dev itself (9 MB/s vs 52 KB/s).
	githubReleaseGyan = "GyanD/codexffmpeg"
	// btbnURL is a rolling release whose asset names do not change.
	btbnURL = "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-gpl.zip"
	// gyanDirectURL is the authoritative host linked from ffmpeg.org. It is
	// tried last because it is frequently throttled to a few tens of KB/s.
	gyanDirectURL = "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
)

// githubAPIBase is a variable so tests can point it at a local server.
var githubAPIBase = "https://api.github.com"

// Sources are tried in order: the smallest archive on the fastest host first,
// the authoritative host last, so a working source is normally found quickly.
func Sources() []Source {
	return []Source{
		{
			Name: "gyan.dev build (GitHub mirror)",
			Resolve: func(ctx context.Context) (string, error) {
				return githubReleaseAsset(ctx, githubReleaseGyan, "-essentials_build.zip")
			},
		},
		{Name: "BtbN/FFmpeg-Builds", URL: btbnURL},
		{Name: "gyan.dev", URL: gyanDirectURL},
	}
}

// Tunables for judging a transfer. They are variables so tests can shrink them.
var (
	// stallTimeout aborts a source that stops sending bytes altogether.
	stallTimeout = 30 * time.Second
	// slowAfter is how long a transfer may run before its average rate is
	// judged; slowMinimum is the rate below which the source is abandoned, so a
	// throttled host is replaced by the next one instead of appearing to hang.
	slowAfter   = 30 * time.Second
	slowMinimum = int64(120 << 10)
	// sourceTimeout bounds a single source, extraction included.
	sourceTimeout = 15 * time.Minute
)

var (
	errTooSlow = errors.New("source is too slow to be useful")
	errStalled = errors.New("source stopped sending data")
)

// Download fetches the first source that works, extracts ffmpeg.exe (and
// ffprobe.exe when present) into destDir and verifies that it runs.
//
// The archive needs random access, so it is streamed to a single predictable
// file in destDir and deleted once ffmpeg is in place. Media being transcribed
// is never written to disk.
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

	url, err := src.address(ctx)
	if err != nil {
		return nil, err
	}

	sctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(sctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cloudtranscribe/0.1 (+ffmpeg bootstrap)")

	// The stall timer is pushed back by every chunk that arrives, so it fires
	// only when the connection goes quiet.
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	resp, err := downloadClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sctx.Err() != nil {
			return nil, errStalled
		}
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	total := resp.ContentLength
	report(prog, Progress{Source: src.Name, Stage: "downloading", Total: total})

	partial := filepath.Join(destDir, archiveName)
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("replace %s: %w", partial, err)
	}
	out, err := os.Create(partial)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", partial, err)
	}

	pw := &progressWriter{
		w:       out,
		prog:    prog,
		source:  src.Name,
		total:   total,
		started: time.Now(),
		stall:   stall,
	}
	_, copyErr := io.Copy(pw, resp.Body)
	closeErr := out.Close()

	switch {
	case errors.Is(copyErr, errTooSlow):
		return nil, fmt.Errorf("%w: %s in %s", errTooSlow,
			HumanRate(pw.rate()), shortDuration(time.Since(pw.started)))
	case copyErr != nil && pw.totalReached():
		// A server that closes cleanly right after the last byte is fine.
	case copyErr != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case copyErr != nil && (sctx.Err() != nil || errors.Is(copyErr, context.DeadlineExceeded)):
		return nil, fmt.Errorf("%w after %s of %s", errStalled,
			HumanBytes(pw.done), HumanBytes(total))
	case copyErr != nil:
		return nil, fmt.Errorf("download interrupted after %s: %w", HumanBytes(pw.done), copyErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}

	info, err := os.Stat(partial)
	if err != nil {
		return nil, err
	}
	if total > 0 && info.Size() != total {
		return nil, fmt.Errorf("truncated download: got %s of %s", HumanBytes(info.Size()), HumanBytes(total))
	}

	archive, err := os.Open(partial)
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

	// The archive has served its purpose; the binaries are what we keep.
	os.Remove(partial)

	return &Install{
		FFmpeg:  ffmpegPath,
		FFprobe: ffprobePath,
		Version: version,
		Origin:  "downloaded from " + src.Name,
	}, nil
}

// archiveName is the fixed name of the in-progress download, so a slow transfer
// can be found in the install folder instead of hiding behind a random name.
const archiveName = "ffmpeg-download.zip"

// downloadClient is tuned for a large one-shot transfer: no overall timeout
// (the context governs instead), and HTTP/1.1 because throttling middleboxes
// and some CDNs stall HTTP/2 bodies in ways that look like a dead connection.
var downloadClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     false,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
		DisableCompression:    true, // zip payloads are already compressed
	},
}

// githubReleaseAsset returns the download address of the first asset of the
// newest release of repo whose file name ends with suffix.
func githubReleaseAsset(ctx context.Context, repo, suffix string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+"/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := downloadClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("look up the newest %s release: %w", repo, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("look up the newest %s release: HTTP %s", repo, resp.Status)
	}

	var payload struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	// The response is small, but a broken one must not be read forever.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("parse the %s release list: %w", repo, err)
	}

	for _, asset := range payload.Assets {
		if strings.HasSuffix(asset.Name, suffix) && asset.URL != "" {
			return asset.URL, nil
		}
	}
	return "", fmt.Errorf("the newest %s release has no %q asset", repo, suffix)
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

// progressWriter reports throughput and gives up on a transfer that is either
// stalled or too slow to finish in a reasonable time.
type progressWriter struct {
	w       io.Writer
	prog    ProgressFunc
	source  string
	total   int64
	done    int64
	started time.Time
	last    time.Time
	stall   *time.Timer
	slow    bool
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.done += int64(n)
		if p.stall != nil {
			p.stall.Reset(stallTimeout)
		}
	}
	if err != nil {
		return n, err
	}

	elapsed := time.Since(p.started)
	if !p.slow && elapsed >= slowAfter && p.rate() < slowMinimum {
		p.slow = true
		return n, errTooSlow
	}
	if p.prog != nil && time.Since(p.last) > 200*time.Millisecond {
		p.last = time.Now()
		p.prog(Progress{
			Source:     p.source,
			Stage:      "downloading",
			Downloaded: p.done,
			Total:      p.total,
			Speed:      p.rate(),
		})
	}
	return n, nil
}

// rate is the average throughput so far, in bytes per second.
func (p *progressWriter) rate() int64 {
	seconds := time.Since(p.started).Seconds()
	if seconds <= 0 {
		return 0
	}
	return int64(float64(p.done) / seconds)
}

// totalReached reports whether the body ended exactly where it said it would.
func (p *progressWriter) totalReached() bool {
	return p.total > 0 && p.done == p.total
}

// HumanBytes formats a size for messages ("117.6 MB").
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n), 0
	for value >= unit && exp < 3 {
		value /= unit
		exp++
	}
	units := [...]string{"KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.1f %s", value, units[exp-1])
}

// HumanRate formats a throughput ("4.1 MB/s").
func HumanRate(bytesPerSecond int64) string {
	if bytesPerSecond <= 0 {
		return "no data"
	}
	return HumanBytes(bytesPerSecond) + "/s"
}

// shortDuration renders 1m5s style durations for messages.
func shortDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}
