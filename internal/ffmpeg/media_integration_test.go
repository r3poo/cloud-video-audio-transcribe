package ffmpeg

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"cloudtranscribe/internal/executil"
)

// TestMediaPipelineAgainstRealFFmpeg exercises the ffmpeg command lines against
// a real binary. It is skipped when no ffmpeg is installed, so it stays opt-in
// for CI machines that do not ship one.
func TestMediaPipelineAgainstRealFFmpeg(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ffmpeg integration test in short mode")
	}
	ctx := context.Background()

	install, ok := Resolve(ctx, "")
	if !ok {
		t.Skip("ffmpeg is not installed; skipping the integration test")
	}

	dir := t.TempDir()
	sample := filepath.Join(dir, "sample.wav")
	writeSample(t, ctx, install, sample)

	info, err := install.Probe(ctx, sample)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Duration < 9.5 || info.Duration > 10.5 {
		t.Errorf("duration = %v, want about 10s", info.Duration)
	}
	if !info.HasAudio {
		t.Error("HasAudio = false for a wav file with one audio stream")
	}

	cuts, err := install.Silences(ctx, sample, -35, 0.6)
	if err != nil {
		t.Fatalf("Silences: %v", err)
	}
	if len(cuts) == 0 {
		t.Fatal("no silence detected in a sample with a 2s gap")
	}
	// The gap sits at 4s..6s, so speech resumes at about 6s.
	var found bool
	for _, c := range cuts {
		if c > 5.5 && c < 6.6 {
			found = true
		}
	}
	if !found {
		t.Errorf("silence ends = %v, want one near 6s", cuts)
	}

	audio, format, err := install.ExtractChunk(ctx, sample, 0, 5)
	if err != nil {
		t.Fatalf("ExtractChunk: %v", err)
	}
	if len(audio) < 5000 {
		t.Errorf("chunk is %d bytes, want a few thousand at least", len(audio))
	}
	if !looksCompressed(audio) {
		t.Errorf("chunk does not start like %s audio: % x", format, audio[:min(4, len(audio))])
	}

	// Seeking into the middle of the file must produce audio too: this is the
	// path used for every chunk after the first.
	mid, _, err := install.ExtractChunk(ctx, sample, 6, 2)
	if err != nil {
		t.Fatalf("ExtractChunk(mid): %v", err)
	}
	if len(mid) < 1000 {
		t.Errorf("mid chunk is %d bytes, want a real payload", len(mid))
	}
}

func writeSample(t *testing.T, ctx context.Context, install *Install, dest string) {
	t.Helper()
	args := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono:d=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-filter_complex", "[0:a][1:a][2:a]concat=n=3:v=0:a=1",
		"-y", dest,
	}
	cmd := exec.CommandContext(ctx, install.FFmpeg, args...)
	executil.Hide(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the sample file: %v\n%s", err, out)
	}
}

// looksCompressed accepts an MP3 frame header or an ID3 tag.
func looksCompressed(audio []byte) bool {
	if len(audio) < 4 {
		return false
	}
	if bytes.HasPrefix(audio, []byte("ID3")) {
		return true
	}
	return audio[0] == 0xFF && audio[1]&0xE0 == 0xE0
}
