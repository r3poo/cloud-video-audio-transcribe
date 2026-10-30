package ffmpeg

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// buildZip renders an in-memory archive that mirrors the layout of the
// published Windows builds: binaries nested in bin/ under a versioned folder.
func buildZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractBinariesFromNestedLayout(t *testing.T) {
	payload := buildZip(t, map[string]string{
		"ffmpeg-9.0.2-essentials_build/bin/ffmpeg.exe":  "ffmpeg-binary",
		"ffmpeg-9.0.2-essentials_build/bin/ffprobe.exe": "ffprobe-binary",
		"ffmpeg-9.0.2-essentials_build/bin/ffplay.exe":  "ffplay-binary",
		"ffmpeg-9.0.2-essentials_build/README.txt":      "docs",
	})

	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}

	dir := t.TempDir()
	ffmpegPath, ffprobePath, err := extractBinaries(reader, dir)
	if err != nil {
		t.Fatalf("extractBinaries: %v", err)
	}

	if ffmpegPath != filepath.Join(dir, "ffmpeg.exe") {
		t.Errorf("ffmpeg path = %q, want it flattened into the destination", ffmpegPath)
	}
	if ffprobePath != filepath.Join(dir, "ffprobe.exe") {
		t.Errorf("ffprobe path = %q", ffprobePath)
	}

	for path, want := range map[string]string{
		ffmpegPath:  "ffmpeg-binary",
		ffprobePath: "ffprobe-binary",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s holds %q, want %q", path, got, want)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "ffplay.exe")); err == nil {
		t.Error("ffplay should not be extracted: only ffmpeg and ffprobe are needed")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); err == nil {
		t.Error("readme should not be extracted")
	}
}

func TestExtractBinariesWithoutFFprobe(t *testing.T) {
	payload := buildZip(t, map[string]string{
		"bin/ffmpeg.exe": "ffmpeg-binary",
	})
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}

	dir := t.TempDir()
	ffmpegPath, ffprobePath, err := extractBinaries(reader, dir)
	if err != nil {
		t.Fatalf("extractBinaries: %v", err)
	}
	if ffmpegPath == "" {
		t.Fatal("ffmpeg was not extracted")
	}
	// No ffprobe in the archive: the extractor may still find one installed
	// elsewhere on the machine, but it must not invent one here.
	if _, err := os.Stat(filepath.Join(dir, "ffprobe.exe")); err == nil {
		t.Error("ffprobe.exe was created although the archive had none")
	}
	if ffprobePath != "" {
		if _, err := os.Stat(ffprobePath); err != nil {
			t.Errorf("reported ffprobe %q does not exist: %v", ffprobePath, err)
		}
		if filepath.Dir(ffprobePath) == dir {
			t.Errorf("ffprobe = %q, want a path outside the extraction folder", ffprobePath)
		}
	}
}

func TestExtractBinariesRejectsAnArchiveWithoutFFmpeg(t *testing.T) {
	payload := buildZip(t, map[string]string{"bin/ffprobe.exe": "ffprobe-binary"})
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatalf("zip reader: %v", err)
	}
	if _, _, err := extractBinaries(reader, t.TempDir()); err == nil {
		t.Fatal("extractBinaries accepted an archive with no ffmpeg")
	}
}

// fakeBinary is a shell script that answers "-version" like ffmpeg does. It
// cannot run on Windows, so the tests that execute it are skipped there; the
// download and extraction logic they cover is platform independent.
func fakeBinary(version string) string {
	return "#!/bin/sh\necho 'ffmpeg version " + version + " Copyright (c) fake'\n"
}

func serveZip(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDownloadFailsOverToTheSecondSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in binary is a shell script")
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer broken.Close()

	payload := buildZip(t, map[string]string{
		"ffmpeg-9.0.2-essentials_build/bin/ffmpeg.exe": fakeBinary("9.0.2-fake"),
	})
	good := serveZip(t, payload)

	dest := t.TempDir()
	var stages []string
	install, err := downloadFirst(context.Background(), []Source{
		{Name: "broken", URL: broken.URL},
		{Name: "good", URL: good.URL},
	}, dest, func(p Progress) { stages = append(stages, p.Stage) })
	if err != nil {
		t.Fatalf("downloadFirst: %v", err)
	}

	if install.Origin != "downloaded from good" {
		t.Errorf("origin = %q, want the second source credited", install.Origin)
	}
	if install.Version != "9.0.2-fake" {
		t.Errorf("version = %q, want the one the binary printed", install.Version)
	}
	if install.FFprobe != "" {
		// A machine-wide ffprobe is acceptable, one extracted from this archive
		// is not: the test archive has none.
		if filepath.Dir(install.FFprobe) == dest {
			t.Errorf("ffprobe = %q, want it outside the download folder", install.FFprobe)
		}
	}
	if _, err := os.Stat(install.FFmpeg); err != nil {
		t.Errorf("ffmpeg was not left in place: %v", err)
	}

	// The archive is a temporary file and must not survive the call.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".zip") {
			t.Errorf("temporary archive %q was left behind", e.Name())
		}
	}

	for _, want := range []string{"requesting", "downloading", "extracting", "verifying"} {
		found := false
		for _, got := range stages {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("progress never reported %q (saw %v)", want, stages)
		}
	}
}

func TestDownloadReportsEveryFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := downloadFirst(context.Background(), []Source{
		{Name: "first", URL: srv.URL},
		{Name: "second", URL: srv.URL + "/missing.zip"},
	}, t.TempDir(), nil)
	if err == nil {
		t.Fatal("downloadFirst reported success with no working source")
	}
	for _, want := range []string{"first", "second", "HTTP 404"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestGithubReleaseAssetPicksTheMatchingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/repos/GyanD/codexffmpeg/releases/latest"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"assets":[
			{"name":"ffmpeg-2026-10-29-git-31ba826726-essentials_build.7z","browser_download_url":"https://example.invalid/a.7z"},
			{"name":"ffmpeg-2026-10-29-git-31ba826726-full_build.zip","browser_download_url":"https://example.invalid/full.zip"},
			{"name":"ffmpeg-2026-10-29-git-31ba826726-essentials_build.zip","browser_download_url":"https://example.invalid/essentials.zip"}
		]}`))
	}))
	defer srv.Close()

	withGithubAPI(t, srv.URL)

	got, err := githubReleaseAsset(context.Background(), githubReleaseGyan, "-essentials_build.zip")
	if err != nil {
		t.Fatalf("githubReleaseAsset: %v", err)
	}
	if got != "https://example.invalid/essentials.zip" {
		t.Fatalf("url = %q, want the essentials archive", got)
	}
}

func TestGithubReleaseAssetExplainsAMissingAsset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"assets":[{"name":"ffmpeg-linux.tar.xz","browser_download_url":"https://example.invalid/l"}]}`))
	}))
	defer srv.Close()

	withGithubAPI(t, srv.URL)

	_, err := githubReleaseAsset(context.Background(), githubReleaseGyan, "-essentials_build.zip")
	if err == nil || !strings.Contains(err.Error(), "essentials_build.zip") {
		t.Fatalf("err = %v, want it to name the asset that is missing", err)
	}
}

// A host that dribbles bytes must be abandoned rather than waited on: this is
// the gyan.dev-at-52KB/s case that looked like a hang to the user.
func TestDownloadAbandonsASlowSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1<<20))
		flusher, _ := w.(http.Flusher)
		chunk := make([]byte, 8<<10)
		for i := 0; i < 8; i++ {
			w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(15 * time.Millisecond)
		}
	}))
	defer srv.Close()

	restore := slowRules(50*time.Millisecond, 50<<20)
	defer restore()

	_, err := downloadFrom(context.Background(), Source{Name: "throttled", URL: srv.URL}, t.TempDir(), nil)
	if !errors.Is(err, errTooSlow) {
		t.Fatalf("err = %v, want errTooSlow", err)
	}
	if !strings.Contains(err.Error(), "/s") {
		t.Errorf("err = %v, want it to report the measured rate", err)
	}
}

func TestDownloadAbandonsAStalledSource(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1<<20))
		w.Write([]byte("PK"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()
	defer close(release)

	restore := slowRules(time.Hour, 0) // only the stall timer may fire
	defer restore()
	withStallTimeout(t, 120*time.Millisecond)

	_, err := downloadFrom(context.Background(), Source{Name: "silent", URL: srv.URL}, t.TempDir(), nil)
	if !errors.Is(err, errStalled) {
		t.Fatalf("err = %v, want errStalled", err)
	}
}

func TestDownloadRejectsATruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(64<<10))
		w.Write([]byte("PK\x03\x04 not the whole archive"))
	}))
	defer srv.Close()

	restore := slowRules(time.Hour, 0)
	defer restore()

	_, err := downloadFrom(context.Background(), Source{Name: "cut", URL: srv.URL}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, want a truncated transfer to be reported", err)
	}
}

func TestHumanBytesAndRate(t *testing.T) {
	sizes := map[int64]string{
		512:         "512 B",
		1500:        "1.5 KB",
		117_600_000: "117.6 MB",
		9_066_944:   "9.1 MB",
	}
	for in, want := range sizes {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	if got := HumanRate(9_066_944); got != "9.1 MB/s" {
		t.Errorf("HumanRate = %q, want %q", got, "9.1 MB/s")
	}
	if got := HumanRate(0); got != "no data" {
		t.Errorf("HumanRate(0) = %q, want %q", got, "no data")
	}
	if got := shortDuration(95 * time.Second); got != "1m35s" {
		t.Errorf("shortDuration = %q, want %q", got, "1m35s")
	}
}

// withGithubAPI points the release lookup at a local server for one test.
func withGithubAPI(t *testing.T, base string) {
	t.Helper()
	old := githubAPIBase
	githubAPIBase = base
	t.Cleanup(func() { githubAPIBase = old })
}

// slowRules shrinks the throughput thresholds for one test.
func slowRules(after time.Duration, minimum int64) func() {
	oldAfter, oldMin := slowAfter, slowMinimum
	slowAfter, slowMinimum = after, minimum
	return func() { slowAfter, slowMinimum = oldAfter, oldMin }
}

// withStallTimeout shrinks the stall watchdog for one test.
func withStallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := stallTimeout
	stallTimeout = d
	t.Cleanup(func() { stallTimeout = old })
}
