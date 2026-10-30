// Package transcribe runs a transcription job end to end: probe the media, split
// it into API-sized spans, send each span to OpenRouter, and write the merged
// result next to the source file.
package transcribe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloudtranscribe/internal/chunk"
	"cloudtranscribe/internal/ffmpeg"
	"cloudtranscribe/internal/openrouter"
	"cloudtranscribe/internal/subtitle"
)

// MediaExtensions are the container and audio formats accepted by the file
// picker. ffmpeg decodes all of them.
var MediaExtensions = []string{
	"mp3", "wav", "m4a", "m4b", "aac", "flac", "ogg", "oga", "opus", "wma", "aiff", "amr", "caf",
	"mp4", "m4v", "mkv", "mov", "avi", "webm", "wmv", "flv", "mpg", "mpeg", "ts", "mts", "3gp", "ogv",
}

// Options control one run.
type Options struct {
	// Model is the OpenRouter model slug, e.g. "openai/whisper-large-v3".
	Model string
	// Language is an optional ISO-639-1 hint such as "en".
	Language string
	// ChunkSeconds is the target span length; zero uses chunk.DefaultTarget.
	ChunkSeconds float64
	// OutputDir overrides where results are written. Empty means next to the
	// input file.
	OutputDir string
}

func (o Options) chunkTarget() float64 {
	if o.ChunkSeconds >= chunk.MinTarget {
		return o.ChunkSeconds
	}
	return chunk.DefaultTarget
}

// Progress is a snapshot reported while a job runs.
type Progress struct {
	// Stage is one of "probing", "planning", "downloading audio", "uploading",
	// "writing".
	Stage string
	// Message is a ready-to-display line.
	Message string
	// Done counts finished spans; Total is the span count for this job.
	Done  int
	Total int
	// Fraction is the overall completion between 0 and 1.
	Fraction float64
}

// Result describes what was written.
type Result struct {
	SRTPath  string
	TextPath string
	// Duration is the source media length.
	Duration time.Duration
	// Spans is the number of requests that were sent.
	Spans int
	// Segments is the number of subtitle cues written.
	Segments int
	// CostUSD is the sum reported by the API, when the provider reports cost.
	CostUSD float64
	// Failed lists spans that could not be transcribed. Results are still
	// written so a long job is not lost to one bad request.
	Failed []chunk.Span
}

// Job binds the dependencies needed for a run.
type Job struct {
	FFmpeg *ffmpeg.Install
	Client *openrouter.Client
	// Progress receives updates; it may be nil.
	Progress func(Progress)
}

// Input is one file to transcribe.
type Input struct {
	Path string
	// Language overrides the job language for this file.
	Language string
}

// Run transcribes one file.
func (j *Job) Run(ctx context.Context, input Input, opts Options) (*Result, error) {
	if j == nil || j.FFmpeg == nil {
		return nil, errors.New("ffmpeg is not available")
	}
	if j.Client == nil {
		return nil, errors.New("OpenRouter client is not configured")
	}
	if strings.TrimSpace(opts.Model) == "" {
		return nil, errors.New("pick a transcription model first")
	}
	if input.Path == "" {
		return nil, errors.New("no input file selected")
	}

	j.report(Progress{Stage: "probing", Message: "Reading media information…"})
	info, err := j.FFmpeg.Probe(ctx, input.Path)
	if err != nil {
		return nil, err
	}
	if info.Duration <= 0 {
		return nil, fmt.Errorf("%s reports no duration; cannot split it for transcription", filepath.Base(input.Path))
	}

	target := opts.chunkTarget()
	var spans []chunk.Span
	if info.Duration > target {
		j.report(Progress{Stage: "planning", Message: "Looking for quiet points to split on…"})
		cuts, err := j.FFmpeg.Silences(ctx, input.Path, -35, 0.6)
		if err != nil {
			// Splitting on the fixed grid is only slightly worse.
			cuts = nil
		}
		spans = chunk.Plan(info.Duration, target, cuts)
	} else {
		spans = []chunk.Span{{Start: 0, End: info.Duration}}
	}

	language := input.Language
	if language == "" {
		language = opts.Language
	}

	result := &Result{Duration: secondsToDuration(info.Duration), Spans: len(spans)}
	var (
		cues     []subtitle.Cue
		texts    []string
		firstErr error
	)

	for idx, span := range spans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		j.report(Progress{
			Stage:    "downloading audio",
			Message:  fmt.Sprintf("Extracting audio for %s…", span),
			Done:     idx,
			Total:    len(spans),
			Fraction: float64(idx) / float64(len(spans)),
		})

		audio, format, err := j.FFmpeg.ExtractChunk(ctx, input.Path, span.Start, span.Duration())
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("audio extraction failed for %s: %w", span, err)
		}

		j.report(Progress{
			Stage:    "uploading",
			Message:  fmt.Sprintf("Transcribing %s (%s)…", span, format),
			Done:     idx,
			Total:    len(spans),
			Fraction: float64(idx) / float64(len(spans)),
		})

		transcript, err := j.transcribe(ctx, opts.Model, language, audio, format)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if firstErr == nil {
				firstErr = err
			}
			result.Failed = append(result.Failed, span)
			continue
		}

		texts = append(texts, transcript.Text)
		result.CostUSD += transcript.Usage.Cost

		spanCues := segmentsToCues(transcript, span)
		if len(spanCues) == 0 && strings.TrimSpace(transcript.Text) != "" {
			spanCues = subtitle.Estimate(transcript.Text, span.Start, span.End, subtitle.DefaultLineWidth)
		}
		cues = append(cues, spanCues...)
	}

	if len(result.Failed) == len(spans) {
		if firstErr == nil {
			firstErr = errors.New("no audio could be transcribed")
		}
		return nil, firstErr
	}

	j.report(Progress{
		Stage:    "writing",
		Message:  "Writing subtitle and text files…",
		Done:     len(spans) - len(result.Failed),
		Total:    len(spans),
		Fraction: 1,
	})

	srtPath, textPath, err := writeOutputs(input.Path, opts.OutputDir, cues, texts)
	if err != nil {
		return nil, err
	}
	result.SRTPath = srtPath
	result.TextPath = textPath
	result.Segments = len(subtitle.Normalize(cues))
	return result, nil
}

// transcribe sends one chunk, falling back to a plain-text response when the
// provider rejects the verbose (timestamped) request format.
func (j *Job) transcribe(ctx context.Context, model, language string, audio []byte, format string) (*openrouter.TranscriptionResult, error) {
	request := openrouter.TranscribeRequest{
		Model:    model,
		Audio:    audio,
		Format:   format,
		Language: language,
		Verbose:  true,
	}
	result, err := j.Client.Transcribe(ctx, request)
	if err == nil {
		return result, nil
	}
	if !rejectsVerboseFormat(err) {
		return nil, err
	}

	request.Verbose = false
	return j.Client.Transcribe(ctx, request)
}

// rejectsVerboseFormat reports whether the failure looks like a model that
// ignores response_format. Timestamps then have to be estimated.
func rejectsVerboseFormat(err error) bool {
	var apiErr *openrouter.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != 400 && apiErr.StatusCode != 404 &&
		apiErr.StatusCode != 422 && apiErr.StatusCode != 500 {
		return false
	}
	text := strings.ToLower(apiErr.Error())
	for _, needle := range []string{"response_format", "verbos", "timestamp", "granularit", "unsupported", "invalid_request"} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// segmentsToCues shifts provider timestamps into the timeline of the whole
// recording.
func segmentsToCues(result *openrouter.TranscriptionResult, span chunk.Span) []subtitle.Cue {
	if len(result.Segments) == 0 {
		return nil
	}
	segments := make([]subtitle.Segment, 0, len(result.Segments))
	for _, s := range result.Segments {
		segments = append(segments, subtitle.Segment{Start: s.Start, End: s.End, Text: s.Text})
	}
	return subtitle.FromSegments(segments, span.Start)
}

// writeOutputs stores the .srt and .txt next to the source, never overwriting
// an existing transcript.
func writeOutputs(inputPath, outputDir string, cues []subtitle.Cue, texts []string) (srtPath, textPath string, err error) {
	dir := outputDir
	if dir == "" {
		dir = filepath.Dir(inputPath)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("create output folder: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(inputPath), filepath.Ext(inputPath))

	srtPath = filepath.Join(dir, base+".srt")
	textPath = filepath.Join(dir, base+".txt")
	// Keep the pair together: if either name is taken, move both to
	// "name (2).srt" / "name (2).txt".
	for i := 2; exists(srtPath) || exists(textPath); i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)", base, i))
		srtPath = candidate + ".srt"
		textPath = candidate + ".txt"
		if i > 1000 {
			break
		}
	}

	if err := os.WriteFile(srtPath, []byte(subtitle.FormatSRT(cues)), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", srtPath, err)
	}
	if err := os.WriteFile(textPath, []byte(formatText(texts)), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", textPath, err)
	}
	return srtPath, textPath, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// formatText renders a readable transcript: one paragraph per chunk, wrapped at
// a comfortable width.
func formatText(chunks []string) string {
	var b strings.Builder
	for _, block := range chunks {
		text := strings.Join(strings.Fields(block), " ")
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(wrap(text, 100))
	}
	b.WriteString("\n")
	return b.String()
}

func wrap(text string, width int) string {
	words := strings.Fields(text)
	var b strings.Builder
	line := 0
	for _, w := range words {
		if line == 0 {
			b.WriteString(w)
			line = len(w)
			continue
		}
		if line+1+len(w) > width {
			b.WriteString("\n")
			b.WriteString(w)
			line = len(w)
			continue
		}
		b.WriteString(" ")
		b.WriteString(w)
		line += 1 + len(w)
	}
	return b.String()
}

func secondsToDuration(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

func (j *Job) report(p Progress) {
	if j.Progress != nil {
		if p.Total > 0 {
			p.Fraction = float64(p.Done) / float64(p.Total)
		}
		j.Progress(p)
	}
}
