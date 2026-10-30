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

	// Every format in the ladder must produce audio the provider can decode,
	// each with the container signature its name promises.
	for _, format := range AudioFormats() {
		audio, err := install.ExtractChunk(ctx, sample, 0, 5, format, 1)
		if err != nil {
			t.Fatalf("ExtractChunk(%s): %v", format.Name, err)
		}
		if len(audio) < 5000 {
			t.Errorf("%s chunk is %d bytes, want a few thousand at least", format.Name, len(audio))
		}
		if !hasMagic(audio, format.Name) {
			t.Errorf("%s chunk does not start like %s audio: % x", format.Name, format.Name, audio[:min(4, len(audio))])
		}
	}

	// Seeking into the middle of the file must produce audio too: this is the
	// path used for every chunk after the first.
	mid, err := install.ExtractChunk(ctx, sample, 6, 2, AudioFormats()[0], 1)
	if err != nil {
		t.Fatalf("ExtractChunk(mid): %v", err)
	}
	if len(mid) < 1000 {
		t.Errorf("mid chunk is %d bytes, want a real payload", len(mid))
	}

	// Speeding the audio up is what halves the bill, so the same span must
	// yield half the samples. Raw PCM makes that exact: 32000 bytes per second
	// at 16 kHz mono 16-bit, whatever the encoder would do with the signal.
	raw := AudioFormat{Name: "pcm", Ext: "raw", Args: []string{"-c:a", "pcm_s16le", "-f", "s16le"}}
	normal, err := install.ExtractChunk(ctx, sample, 0, 4, raw, 1)
	if err != nil {
		t.Fatalf("ExtractChunk(1x): %v", err)
	}
	doubled, err := install.ExtractChunk(ctx, sample, 0, 4, raw, 2)
	if err != nil {
		t.Fatalf("ExtractChunk(2x): %v", err)
	}
	if want := 4 * 32000; len(normal) < want*9/10 || len(normal) > want*11/10 {
		t.Errorf("4s of PCM is %d bytes, want about %d", len(normal), want)
	}
	ratio := float64(len(doubled)) / float64(len(normal))
	if ratio < 0.45 || ratio > 0.55 {
		t.Errorf("2x chunk is %.2f of the 1x chunk, want about half", ratio)
	}

	// 3x needs a two-stage atempo chain, which is where a naive filter breaks.
	tripled, err := install.ExtractChunk(ctx, sample, 0, 4, raw, 3)
	if err != nil {
		t.Fatalf("ExtractChunk(3x): %v", err)
	}
	if ratio := float64(len(tripled)) / float64(len(normal)); ratio < 0.28 || ratio > 0.39 {
		t.Errorf("3x chunk is %.2f of the 1x chunk, want about a third", ratio)
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
func hasMagic(audio []byte, format string) bool {
	if len(audio) < 4 {
		return false
	}
	switch format {
	case "flac":
		return bytes.HasPrefix(audio, []byte("fLaC"))
	case "wav":
		return bytes.HasPrefix(audio, []byte("RIFF"))
	case "pcm16":
		// Raw PCM has no header; any substantial payload is the right answer.
		return len(audio) > 5000
	case "mp3":
		if bytes.HasPrefix(audio, []byte("ID3")) {
			return true
		}
		return audio[0] == 0xFF && audio[1]&0xE0 == 0xE0
	}
	return false
}
