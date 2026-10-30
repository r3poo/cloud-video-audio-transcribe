package ffmpeg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// AudioFormat is one container and codec combination that can be sent to the
// API. Name is the value for input_audio.format and must match the bytes.
type AudioFormat struct {
	Name string
	Ext  string
	// Args are ffmpeg's output options for this container.
	Args []string
}

// AudioFormats lists the encodings to offer the API, in the order they are
// tried. Every one is mono, 16 kHz, which is what speech models expect.
//
// WAV comes first. It is what OpenRouter's own docs call "the safest default",
// it needs no decoder beyond a RIFF header, and it is the format the provider
// behind most speech-to-text endpoints documents. MP3 follows: smaller over the
// wire and accepted nearly everywhere, but every provider bundles a different
// decoder. FLAC is lossless and compact yet less universally accepted, and raw
// PCM16 comes last because only Riva-style backends take it. OpenRouter exposes
// no per-model list of accepted containers, so the order is a guess and every
// rejection is answered by the next entry.
func AudioFormats() []AudioFormat {
	return []AudioFormat{
		{Name: "wav", Ext: "wav", Args: []string{"-c:a", "pcm_s16le", "-f", "wav"}},
		{Name: "mp3", Ext: "mp3", Args: []string{"-c:a", "libmp3lame", "-b:a", "64k", "-f", "mp3"}},
		{Name: "flac", Ext: "flac", Args: []string{"-c:a", "flac", "-compression_level", "8", "-f", "flac"}},
		{Name: "pcm16", Ext: "pcm", Args: []string{"-c:a", "pcm_s16le", "-f", "s16le"}},
	}
}

// FindAudioFormat returns the named encoding, and the first one when the name
// is unknown.
func FindAudioFormat(name string) AudioFormat {
	formats := AudioFormats()
	for _, f := range formats {
		if f.Name == name {
			return f
		}
	}
	return formats[0]
}

// ExtractChunk decodes one span of the input and returns the audio bytes in the
// requested format. Nothing touches the filesystem: ffmpeg writes to stdout.
//
// speed above 1 shortens the clip with atempo, which compresses time without
// changing pitch. The API bills by audio seconds, so a 2× speed halves the cost
// of every chunk.
func (i *Install) ExtractChunk(ctx context.Context, file string, start, duration float64, format AudioFormat, speed float64) ([]byte, error) {
	args := []string{"-hide_banner", "-nostdin", "-nostats", "-loglevel", "error"}
	if start > 0 {
		// Input seeking keeps chunk extraction fast; audio seeking is sample
		// accurate in current ffmpeg releases.
		args = append(args, "-ss", strconv.FormatFloat(start, 'f', 3, 64))
	}
	if duration > 0 {
		// -t before -i limits how much of the source is read, not how much
		// output is produced. That distinction matters with atempo: as an
		// output option it would keep reading source until it had duration
		// seconds of sped-up audio, which silently doubles the billed span.
		args = append(args, "-t", strconv.FormatFloat(duration, 'f', 3, 64))
	}
	args = append(args, "-i", file)
	args = append(args,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-ac", "1", "-ar", "16000",
	)
	if filter := tempoFilter(speed); filter != "" {
		args = append(args, "-af", filter)
	}
	args = append(args, format.Args...)
	args = append(args, "pipe:1")

	out, stderr, err := i.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("%s encoding failed: %w (%s)", format.Name, err, detail(stderr))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s encoding produced no audio for this span (%s)", format.Name, detail(stderr))
	}
	return out, nil
}

// detail renders ffmpeg's complaint as one short line, so a failure explains
// itself instead of pointing at an exit code.
func detail(stderr string) string {
	s := strings.TrimSpace(stderr)
	if s == "" {
		return "no diagnostics"
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// tempoFilter renders the atempo chain for a playback speed. One atempo stage
// accepts 0.5–2.0, so larger factors are applied in stages: 3× becomes
// atempo=2,atempo=1.5.
func tempoFilter(speed float64) string {
	if !(speed > 0) || math.Abs(speed-1) < 1e-9 {
		return ""
	}

	remaining := speed
	var stages []string
	for remaining > 2 && len(stages) < 8 {
		stages = append(stages, "atempo=2")
		remaining /= 2
	}
	for remaining < 0.5 && len(stages) < 8 {
		stages = append(stages, "atempo=0.5")
		remaining /= 0.5
	}
	stages = append(stages, fmt.Sprintf("atempo=%.5g", remaining))
	return strings.Join(stages, ",")
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
