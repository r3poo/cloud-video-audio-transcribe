// Package transcribe runs a transcription job end to end: probe the media, split
// it into API-sized spans, send each span to OpenRouter, and write the merged
// result next to the source file.
package transcribe

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	// Speed plays the audio faster before sending it. Providers bill by audio
	// second, so 2 halves the cost of every chunk. Zero or out-of-range values
	// mean normal speed.
	Speed float64
	// FirstFormat names the container to try before the rest of the ladder, so a
	// model that only accepts one encoding does not pay for a rejection on every
	// run. Empty means the built-in order.
	FirstFormat string
}

// Speed limits. Below 0.5 speech drags, above 4 the models start dropping
// words, and both cost accuracy rather than just money.
const (
	MinSpeed = 0.5
	MaxSpeed = 4.0
)

// speedValue is the factor actually used for extraction.
func (o Options) speedValue() float64 {
	if !(o.Speed >= MinSpeed && o.Speed <= MaxSpeed) {
		return 1
	}
	return o.Speed
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
	// AudioFormat is the container the provider accepted, which is worth
	// remembering: the next run can start with it.
	AudioFormat string
	// TimestampsEstimated is set when the provider refused timestamped output,
	// so cue times were spaced by text length instead of being measured.
	TimestampsEstimated bool
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

	speed := opts.speedValue()
	if speed != 1 {
		j.report(Progress{
			Stage:   "probing",
			Message: fmt.Sprintf("Playing the audio at %s× so the model is billed for less of it…", trimFloat(speed)),
		})
	}

	result := &Result{Duration: secondsToDuration(info.Duration), Spans: len(spans)}
	var (
		cues     []subtitle.Cue
		texts    []string
		firstErr error
	)
	// preferred carries the container the provider accepted from one span to the
	// next, so a rejection costs one retry for the whole job rather than one per
	// chunk.
	preferred := opts.FirstFormat
	// verbose does the same for the response shape, which a model may refuse
	// independently of the audio.
	verbose := true

	for idx, span := range spans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		report := func(p Progress) {
			p.Done, p.Total = idx, len(spans)
			p.Fraction = float64(idx) / float64(len(spans))
			j.report(p)
		}

		piece, err := j.transcribeSpan(ctx, input.Path, span, language, opts, &preferred, &verbose, report, 0)
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

		texts = append(texts, piece.texts...)
		cues = append(cues, piece.cues...)
		result.CostUSD += piece.cost
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
	result.AudioFormat = preferred
	result.TimestampsEstimated = !verbose
	result.Segments = len(subtitle.Normalize(cues))
	return result, nil
}

// transcribe sends one chunk with the requested response shape.
func (j *Job) transcribe(ctx context.Context, model, language string, audio []byte, format string, verbose bool) (*openrouter.TranscriptionResult, error) {
	return j.Client.Transcribe(ctx, openrouter.TranscribeRequest{
		Model:    model,
		Audio:    audio,
		Format:   format,
		Language: language,
		Verbose:  verbose,
	})
}

// requestRefused reports whether the provider refused the request itself, the
// class of failure a different request shape can fix. The message does not say
// which field it disliked: asking for timestamped output a route cannot produce
// is answered with the same generic 400 — "could not process the audio input" —
// that an unusable container gets, so the two are told apart by trying the
// cheaper hypothesis, not by reading the text.
func requestRefused(err error) bool {
	var apiErr *openrouter.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound,
		http.StatusUnprocessableEntity, http.StatusInternalServerError:
		return true
	}
	return false
}

// maxPayloadBytes is the largest audio chunk sent in one request. Two ceilings
// sit above it: OpenRouter rejects an inline base64 body past the 25 MB multipart
// cap on every provider except OpenAI and Groq, and the providers themselves cap
// uploads higher still (50 MiB on DeepInfra). Base64 adds a third to the size, so
// 17 MiB of audio becomes a ~22.7 MiB body, which clears all three with room to
// spare. A span that would exceed it is split before uploading.
const maxPayloadBytes = 17 << 20

// maxSplitDepth bounds the halving, so a pathological file cannot explode into
// hundreds of requests: 600s at depth 0 reaches 75s at depth 3.
const maxSplitDepth = 3

// chunkPiece is what one span, or one half of a split span, contributes.
type chunkPiece struct {
	texts []string
	cues  []subtitle.Cue
	cost  float64
}

func (p *chunkPiece) merge(other *chunkPiece) {
	p.texts = append(p.texts, other.texts...)
	p.cues = append(p.cues, other.cues...)
	p.cost += other.cost
}

// transcribeSpan extracts one span and sends it. Three failures have a defined
// remedy: a provider that calls the audio malformed is answered with the next
// encoding, a request shape that is refused is re-sent plain, and a payload that
// is too large for one request is halved.
func (j *Job) transcribeSpan(ctx context.Context, path string, span chunk.Span, language string, opts Options, preferred *string, verbose *bool, report func(Progress), depth int) (*chunkPiece, error) {
	speed := opts.speedValue()
	formats := orderedFormats(*preferred)

	// split halves the span and transcribes each part, reusing the job's
	// preferences so the half that worked first is tried first again.
	canSplit := span.Duration() > 2*chunk.MinTarget && depth < maxSplitDepth
	split := func() (*chunkPiece, error) {
		mid := span.Start + span.Duration()/2
		piece := &chunkPiece{}
		for _, half := range []chunk.Span{{Start: span.Start, End: mid}, {Start: mid, End: span.End}} {
			part, err := j.transcribeSpan(ctx, path, half, language, opts, preferred, verbose, report, depth+1)
			if err != nil {
				return nil, err
			}
			piece.merge(part)
		}
		return piece, nil
	}

	var lastErr error
	var attempted []string
	for attempt := 0; attempt < len(formats); attempt++ {
		format := formats[attempt]
		attempted = append(attempted, format.Name)

		report(Progress{
			Stage:   "downloading audio",
			Message: fmt.Sprintf("Extracting %s audio for %s…", format.Name, span),
		})
		audio, err := j.FFmpeg.ExtractChunk(ctx, path, span.Start, span.Duration(), format, speed)
		if err != nil {
			return nil, fmt.Errorf("audio extraction failed for %s: %w", span, err)
		}

		// Cheaper to split than to upload something the gateway will reject.
		if len(audio) > maxPayloadBytes && canSplit {
			return split()
		}

		report(Progress{
			Stage:   "uploading",
			Message: fmt.Sprintf("Transcribing %s as %s…", span, format.Name),
		})
		result, err := j.transcribe(ctx, opts.Model, language, audio, format.Name, *verbose)
		if err != nil && *verbose && ctx.Err() == nil && requestRefused(err) {
			// Before paying for a re-encode, check the cheaper explanation: the
			// route may refuse timestamped output rather than this encoding.
			report(Progress{
				Stage:   "uploading",
				Message: fmt.Sprintf("%s as %s was refused · retrying the same audio without timestamps", span, format.Name),
			})
			result, err = j.transcribe(ctx, opts.Model, language, audio, format.Name, false)
			// A plain refusal settles the question for the rest of the job.
			*verbose = false
		}
		if err == nil {
			*preferred = format.Name
			return &chunkPiece{
				texts: []string{result.Text},
				cues:  cuesFor(result, span, speed),
				cost:  result.Usage.Cost,
			}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A size estimate can be wrong — an unexpected container, a provider
		// with a lower cap than documented. Split rather than lose the span.
		if tooLarge(err) && canSplit {
			report(Progress{
				Stage:   "uploading",
				Message: fmt.Sprintf("%s was too large for one request · splitting it in half", span),
			})
			return split()
		}
		if !rejectsAudio(err) {
			return nil, err
		}

		lastErr = fmt.Errorf("%s audio was rejected by the provider: %w", format.Name, err)
		if attempt+1 < len(formats) {
			report(Progress{
				Stage:   "uploading",
				Message: fmt.Sprintf("%s · retrying %s as %s", lastErr, span, formats[attempt+1].Name),
			})
		}
	}
	if lastErr == nil {
		return nil, fmt.Errorf("no audio could be extracted from %s", span)
	}
	// Every container was refused, so this model cannot take the audio at all
	// and the honest advice is to switch models rather than to retry.
	return nil, fmt.Errorf("the provider rejected %s audio for %s; try a different transcription model — %w",
		strings.Join(attempted, ", "), span, lastErr)
}

// orderedFormats puts the container that worked last time in front. Preference
// is only a hint: the rest of the ladder stays behind it, so a model that
// changes behaviour or a file that needs another encoding still succeeds.
func orderedFormats(first string) []ffmpeg.AudioFormat {
	formats := ffmpeg.AudioFormats()
	if first == "" {
		return formats
	}
	for i, format := range formats {
		if format.Name != first || i == 0 {
			continue
		}
		ordered := make([]ffmpeg.AudioFormat, 0, len(formats))
		ordered = append(ordered, format)
		ordered = append(ordered, formats[:i]...)
		return append(ordered, formats[i+1:]...)
	}
	return formats
}

// tooLarge reports whether the request was refused for its size, which is the
// one failure that splitting the span fixes. Gateways answer 413, and providers
// behind them phrase it their own way, so the text is checked as well.
func tooLarge(err error) bool {
	var apiErr *openrouter.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	text := strings.ToLower(apiErr.Error())
	for _, needle := range []string{"too large", "payload too", "exceeds the maximum", "file size"} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// rejectsAudio reports whether the provider refused the audio itself, which is
// the one failure a different container can fix. Providers phrase it as
// "unsupported or malformed audio" or "could not process the audio input" and
// answer with 400 (or 415); the message always mentions audio.
func rejectsAudio(err error) bool {
	var apiErr *openrouter.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != http.StatusBadRequest && apiErr.StatusCode != http.StatusUnsupportedMediaType {
		return false
	}
	text := strings.ToLower(apiErr.Error())
	if !strings.Contains(text, "audio") {
		return false
	}
	for _, needle := range []string{
		"malformed", "unsupported", "not supported", "could not process", "cannot process",
		"invalid", "decode", "corrupt", "truncated", "empty",
	} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

// cuesFor turns a transcript into subtitle cues on the original timeline.
func cuesFor(result *openrouter.TranscriptionResult, span chunk.Span, speed float64) []subtitle.Cue {
	cues := segmentsToCues(result, span, speed)
	if len(cues) == 0 && strings.TrimSpace(result.Text) != "" {
		cues = subtitle.Estimate(result.Text, span.Start, span.End, subtitle.DefaultLineWidth)
	}
	return cues
}

// segmentsToCues shifts provider timestamps into the timeline of the whole
// recording. When the audio was sped up, the model's timestamps describe the
// shortened clip, so they are stretched back by the same factor.
func segmentsToCues(result *openrouter.TranscriptionResult, span chunk.Span, speed float64) []subtitle.Cue {
	if len(result.Segments) == 0 {
		return nil
	}
	if !(speed > 0) {
		speed = 1
	}
	segments := make([]subtitle.Segment, 0, len(result.Segments))
	for _, s := range result.Segments {
		segments = append(segments, subtitle.Segment{
			Start: s.Start * speed,
			End:   s.End * speed,
			Text:  s.Text,
		})
	}
	return subtitle.FromSegments(segments, span.Start)
}

// trimFloat renders a factor without trailing zeros: 1.5, 2, 1.25.
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
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
