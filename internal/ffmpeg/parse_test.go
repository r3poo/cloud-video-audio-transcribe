package ffmpeg

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "gyan build",
			output: "ffmpeg version 7.1.1-full_build-www.gyan.dev Copyright (c) 2000-2024\nbuilt with gcc\n",
			want:   "7.1.1-full_build-www.gyan.dev",
		},
		{
			name:   "distro build",
			output: "ffmpeg version 6.1.1 Copyright (c) 2000-2023 the FFmpeg developers\n",
			want:   "6.1.1",
		},
		{
			name:   "empty output",
			output: "",
			want:   "",
		},
		{
			name:   "unrelated banner",
			output: "ffmpeg: command not found\n",
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseVersion(tc.output); got != tc.want {
				t.Errorf("ParseVersion = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseDurationBanner(t *testing.T) {
	stderr := `Input #0, mov,mp4,m4a,3gp,3g2,mj2, from 'talk.mp4':
  Duration: 00:01:23.45, start: 0.000000, bitrate: 1200 kb/s
  Stream #0:0(und): Video: h264
  Stream #0:1(und): Audio: aac (LC), 44100 Hz, stereo
`
	if got := parseDurationBanner(stderr); got != 83.45 {
		t.Errorf("parseDurationBanner = %v, want 83.45", got)
	}
	if long := parseDurationBanner("Duration: 02:00:00.00, start: 0"); long != 7200 {
		t.Errorf("parseDurationBanner hours = %v, want 7200", long)
	}
	if got := parseDurationBanner("Duration: N/A, start: 0.000000"); got != 0 {
		t.Errorf("parseDurationBanner(N/A) = %v, want 0", got)
	}
	if got := parseDurationBanner("no banner here"); got != 0 {
		t.Errorf("parseDurationBanner(garbage) = %v, want 0", got)
	}
}

func TestParseHasAudioBanner(t *testing.T) {
	withAudio := "Stream #0:1(und): Audio: aac (LC), 44100 Hz, stereo, fltp"
	if !parseHasAudioBanner(withAudio) {
		t.Error("audio stream was not detected")
	}
	videoOnly := "Stream #0:0(und): Video: h264 (High), yuv420p"
	if parseHasAudioBanner(videoOnly) {
		t.Error("video-only stream reported audio")
	}
}

func TestParseSilenceEnds(t *testing.T) {
	stderr := `[silencedetect @ 0x1] silence_start: 12.3
[silencedetect @ 0x1] silence_end: 15.7 | silence_duration: 3.4
[silencedetect @ 0x1] silence_start: 30
[silencedetect @ 0x1] silence_end: 31.25 | silence_duration: 1.25
`
	got := ParseSilenceEnds(stderr)
	want := []float64{15.7, 31.25}
	if len(got) != len(want) {
		t.Fatalf("ParseSilenceEnds = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
	if len(ParseSilenceEnds("nothing")) != 0 {
		t.Error("expected no cuts from unrelated output")
	}
}

func TestExtractBinaries(t *testing.T) {
	archive := zipArchive(t, map[string]string{
		"ffmpeg-7.1.1-full_build/bin/ffmpeg.exe":  "ffmpeg-binary",
		"ffmpeg-7.1.1-full_build/bin/ffprobe.exe": "ffprobe-binary",
		"ffmpeg-7.1.1-full_build/README.txt":      "docs",
		"ffmpeg-7.1.1-full_build/bin/weird.dat":   "ignored",
	})

	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	ffmpegPath, ffprobePath, err := extractBinaries(reader, dest)
	if err != nil {
		t.Fatalf("extractBinaries: %v", err)
	}
	if ffmpegPath != filepath.Join(dest, "ffmpeg.exe") {
		t.Errorf("ffmpeg path = %q", ffmpegPath)
	}
	if ffprobePath != filepath.Join(dest, "ffprobe.exe") {
		t.Errorf("ffprobe path = %q", ffprobePath)
	}
	data, err := os.ReadFile(ffmpegPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ffmpeg-binary" {
		t.Errorf("ffmpeg content = %q", data)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.txt")); !os.IsNotExist(err) {
		t.Error("archive extras were extracted")
	}
}

func TestExtractBinariesRejectsArchiveWithoutFFmpeg(t *testing.T) {
	archive := zipArchive(t, map[string]string{"README.txt": "docs"})
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := extractBinaries(reader, t.TempDir()); err == nil {
		t.Fatal("expected an error for an archive without ffmpeg.exe")
	}
}

func TestMissingEncoder(t *testing.T) {
	stderr := "Unknown encoder 'libmp3lame'"
	if !missingEncoder(stderr) {
		t.Error("missing encoder was not recognised")
	}
	if missingEncoder("Error opening input file") {
		t.Error("unrelated error reported a missing encoder")
	}
}

func zipArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
