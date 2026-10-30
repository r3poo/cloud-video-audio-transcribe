package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloudtranscribe/internal/executil"
)

// MediaInfo is what the pipeline needs to know before chunking.
type MediaInfo struct {
	Duration   float64
	HasAudio   bool
	AudioCodec string
}

// Probe determines the duration and audio presence of a file. ffprobe is used
// when available; otherwise ffmpeg's own stderr output is parsed, so a host
// with only ffmpeg.exe still works.
func (i *Install) Probe(ctx context.Context, file string) (*MediaInfo, error) {
	if i.HasFFprobe() {
		if info, err := i.probeWithFFprobe(ctx, file); err == nil {
			if info.HasAudio {
				return info, nil
			}
			return nil, fmt.Errorf("%s has no audio stream", file)
		}
	}

	info, err := i.probeWithFFmpeg(ctx, file)
	if err != nil {
		return nil, err
	}
	if !info.HasAudio {
		return nil, fmt.Errorf("%s has no audio stream", file)
	}
	return info, nil
}

func (i *Install) probeWithFFprobe(ctx context.Context, file string) (*MediaInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, i.FFprobe,
		"-v", "error",
		"-show_entries", "format=duration:stream=codec_type,codec_name",
		"-of", "json",
		file,
	)
	executil.Hide(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe: %w: %s", err, brief(stderr.String()))
	}

	var payload struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		return nil, fmt.Errorf("decode ffprobe output: %w", err)
	}

	info := &MediaInfo{}
	if d, err := strconv.ParseFloat(payload.Format.Duration, 64); err == nil {
		info.Duration = d
	}
	for _, s := range payload.Streams {
		if s.CodecType == "audio" {
			info.HasAudio = true
			info.AudioCodec = s.CodecName
			break
		}
	}
	return info, nil
}

func (i *Install) probeWithFFmpeg(ctx context.Context, file string) (*MediaInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// ffmpeg -i with no output is expected to fail; the metadata we need is in
	// the stderr banner either way.
	cmd := exec.CommandContext(ctx, i.FFmpeg, "-hide_banner", "-nostdin", "-i", file)
	executil.Hide(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()

	out := stderr.String()
	info := &MediaInfo{
		Duration: parseDurationBanner(out),
		HasAudio: parseHasAudioBanner(out),
	}
	if info.Duration == 0 && !info.HasAudio {
		return nil, fmt.Errorf("could not read media information from %s", file)
	}
	return info, nil
}

var (
	durationBannerRE = regexp.MustCompile(`Duration:\s*(\d+):(\d{2}):(\d{2}(?:\.\d+)?)`)
	audioStreamRE    = regexp.MustCompile(`Stream #\d+:\d+[^:]*:\s*Audio:`)
	silenceEndRE     = regexp.MustCompile(`silence_end:\s*([0-9]+(?:\.[0-9]+)?)`)
)

// parseDurationBanner extracts a duration in seconds from ffmpeg's
// "Duration: 00:01:23.45" banner line. It returns 0 when absent or "N/A".
func parseDurationBanner(stderr string) float64 {
	m := durationBannerRE.FindStringSubmatch(stderr)
	if m == nil {
		return 0
	}
	hours, _ := strconv.Atoi(m[1])
	minutes, _ := strconv.Atoi(m[2])
	seconds, _ := strconv.ParseFloat(m[3], 64)
	return float64(hours)*3600 + float64(minutes)*60 + seconds
}

// parseHasAudioBanner reports whether ffmpeg listed an audio stream.
func parseHasAudioBanner(stderr string) bool {
	return audioStreamRE.MatchString(stderr)
}

// ParseSilenceEnds extracts the timestamps where detected silence ends, i.e.
// the points where speech resumes. Those make good chunk boundaries.
func ParseSilenceEnds(stderr string) []float64 {
	matches := silenceEndRE.FindAllStringSubmatch(stderr, -1)
	out := make([]float64, 0, len(matches))
	for _, m := range matches {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil {
			out = append(out, f)
		}
	}
	return out
}

// Silences runs ffmpeg's silencedetect filter and returns the end timestamps of
// every silence longer than minSilence seconds. It is best effort: callers
// should fall back to fixed boundaries when it fails.
func (i *Install) Silences(ctx context.Context, file string, noiseDB, minSilence float64) ([]float64, error) {
	args := []string{
		"-hide_banner", "-nostdin", "-nostats", "-loglevel", "info",
		"-i", file,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-af", fmt.Sprintf("silencedetect=noise=%.0fdB:d=%.2f", noiseDB, minSilence),
		"-f", "null", "-",
	}
	_, stderr, err := i.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return ParseSilenceEnds(stderr), nil
}

// ExtractChunk decodes one span of the input and returns compressed audio bytes
// plus the format name to send to the API. Nothing touches the filesystem:
// ffmpeg writes to its stdout.
//
// Audio is downmixed to mono 16 kHz, which is what speech models expect and
// keeps a ten minute chunk around 5 MB.
func (i *Install) ExtractChunk(ctx context.Context, file string, start, duration float64) ([]byte, string, error) {
	type encoding struct {
		format string
		codec  []string
	}
	encodings := []encoding{
		{"mp3", []string{"-c:a", "libmp3lame", "-b:a", "64k", "-f", "mp3"}},
		// Some builds ship without libmp3lame; AAC/ADTS is accepted by the API.
		{"aac", []string{"-c:a", "aac", "-b:a", "96k", "-f", "adts"}},
	}

	var lastErr error
	for attempt, enc := range encodings {
		args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error"}
		if start > 0 {
			// Input seeking keeps chunk extraction fast; audio seeking is
			// sample accurate in current ffmpeg releases.
			args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
		}
		args = append(args, "-i", file)
		if duration > 0 {
			args = append(args, "-t", strconv.FormatFloat(duration, 'f', 3, 64))
		}
		args = append(args,
			"-map", "0:a:0", "-vn", "-sn", "-dn",
			"-ac", "1", "-ar", "16000",
		)
		args = append(args, enc.codec...)
		args = append(args, "pipe:1")

		out, stderr, err := i.run(ctx, args...)
		if err == nil && len(out) > 0 {
			return out, enc.format, nil
		}
		if err == nil {
			err = fmt.Errorf("ffmpeg produced no audio for this span")
		}
		lastErr = fmt.Errorf("%s encoding failed: %w", enc.format, err)

		// Only a missing encoder justifies trying the next encoding; any other
		// failure (bad file, no audio stream) would fail identically.
		if attempt+1 < len(encodings) && !missingEncoder(stderr) {
			return nil, "", lastErr
		}
	}
	return nil, "", lastErr
}

func missingEncoder(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "unknown encoder") ||
		strings.Contains(s, "encoder not found") ||
		strings.Contains(s, "unknown codec") ||
		strings.Contains(s, "requested output format") ||
		strings.Contains(s, "experimental")
}

// run executes ffmpeg and returns stdout, stderr and the exit status.
func (i *Install) run(ctx context.Context, args ...string) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, i.FFmpeg, args...)
	executil.Hide(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, stderr.String(), ctx.Err()
		}
		return nil, stderr.String(), fmt.Errorf("%w: %s", err, brief(stderr.String()))
	}
	return stdout.Bytes(), stderr.String(), nil
}

// brief trims ffmpeg's multi-line diagnostics to something a dialog can show.
func brief(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	var kept []string
	for _, l := range lines {
		if s := strings.TrimSpace(l); s != "" {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return "no diagnostic output"
	}
	if len(kept) > 3 {
		kept = kept[len(kept)-3:]
	}
	return strings.Join(kept, " | ")
}
