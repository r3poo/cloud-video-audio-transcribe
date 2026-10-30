package transcribe

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudtranscribe/internal/chunk"
	"cloudtranscribe/internal/ffmpeg"
	"cloudtranscribe/internal/openrouter"
	"cloudtranscribe/internal/subtitle"
)

// The generic 400 a provider returns for an unsupported request shape carries no
// hint about the field it disliked: OpenRouter's gateway reuses
// "could not process the audio input" for both a bad container and a bad option.
// So the retry rule is driven by the status alone.
func TestRequestRefused(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limited", &openrouter.APIError{StatusCode: 429, Message: "slow down"}, false},
		{"auth", &openrouter.APIError{StatusCode: 401, Message: "invalid key"}, false},
		{"payment", &openrouter.APIError{StatusCode: 402, Message: "insufficient credits"}, false},
		{"forbidden", &openrouter.APIError{StatusCode: 403, Message: "spend limit"}, false},
		{"upstream failed", &openrouter.APIError{StatusCode: 502, Message: "upstream audio failed"}, false},
		{"400 names the format", &openrouter.APIError{StatusCode: 400, Message: "unsupported response_format"}, true},
		{"422 names timestamps", &openrouter.APIError{StatusCode: 422, Message: "timestamp_granularities not supported"}, true},
		{"400 generic", &openrouter.APIError{StatusCode: 400, Message: "bad request"}, true},
		{"400 blames the audio", &openrouter.APIError{StatusCode: 400, Message: "Provider could not process the audio input (unsupported or malformed audio)"}, true},
		{"joined", errors.Join(errors.New("ctx"), &openrouter.APIError{StatusCode: 400, Message: "verbosity unsupported"}), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestRefused(tc.err); got != tc.want {
				t.Errorf("requestRefused(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRequestRefusedThroughWrap(t *testing.T) {
	wrapped := error(&wrappedErr{inner: &openrouter.APIError{StatusCode: 400, Message: "response_format invalid"}})
	if !requestRefused(wrapped) {
		t.Error("errors.As should see the APIError through the wrapper")
	}
}

type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }

func TestSegmentsToCuesAppliesOffset(t *testing.T) {
	result := &openrouter.TranscriptionResult{
		Segments: []openrouter.Segment{
			{Start: 0.5, End: 1.5, Text: "hi"},
			{Start: 2, End: 3, Text: "there"},
		},
	}
	cues := segmentsToCues(result, chunk.Span{Start: 600, End: 1200}, 1)
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(cues))
	}
	if cues[0].Start != 600.5 || cues[1].End != 603 {
		t.Errorf("cues not shifted onto the recording timeline: %v", cues)
	}

	if got := segmentsToCues(&openrouter.TranscriptionResult{}, chunk.Span{}, 1); got != nil {
		t.Errorf("segmentsToCues with no segments = %v, want nil", got)
	}
}

func TestWriteOutputs(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "talk.mp4")
	if err := os.WriteFile(input, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}

	cues := []subtitle.Cue{{Start: 0, End: 1, Text: "hello"}}
	srt, txt, err := writeOutputs(input, "", cues, []string{"hello"})
	if err != nil {
		t.Fatalf("writeOutputs: %v", err)
	}
	if srt != filepath.Join(dir, "talk.srt") || txt != filepath.Join(dir, "talk.txt") {
		t.Errorf("paths = %q, %q, want them next to the input", srt, txt)
	}
	data, err := os.ReadFile(srt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "00:00:00,000 --> 00:00:01,000") {
		t.Errorf("srt content:\n%s", data)
	}
	text, err := os.ReadFile(txt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(text)) != "hello" {
		t.Errorf("txt content = %q", text)
	}
}

func TestWriteOutputsNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "talk.wav")
	if err := os.WriteFile(input, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "talk.srt"), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	srt, txt, err := writeOutputs(input, "", []subtitle.Cue{{Start: 0, End: 1, Text: "hi"}}, nil)
	if err != nil {
		t.Fatalf("writeOutputs: %v", err)
	}
	if srt != filepath.Join(dir, "talk (2).srt") || txt != filepath.Join(dir, "talk (2).txt") {
		t.Errorf("paths = %q, %q, want the numbered pair", srt, txt)
	}
	data, err := os.ReadFile(filepath.Join(dir, "talk.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing" {
		t.Errorf("existing transcript was modified: %q", data)
	}
}

func TestWriteOutputsCreatesOutputDir(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "clip.mp3")
	if err := os.WriteFile(input, []byte("fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "new", "nested")

	srt, _, err := writeOutputs(input, out, nil, nil)
	if err != nil {
		t.Fatalf("writeOutputs: %v", err)
	}
	if filepath.Dir(srt) != out {
		t.Errorf("srt written to %q, want %q", filepath.Dir(srt), out)
	}
}

func TestFormatText(t *testing.T) {
	got := formatText([]string{"first   chunk  text", "", "second chunk"})
	want := "first chunk text\n\nsecond chunk\n"
	if got != want {
		t.Errorf("formatText = %q, want %q", got, want)
	}
}

func TestFormatTextWraps(t *testing.T) {
	words := strings.Repeat("word ", 40)
	got := formatText([]string{words})
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if len(line) > 100 {
			t.Errorf("line of %d chars exceeds the wrap width: %q", len(line), line)
		}
	}
	if strings.Count(got, "\n") < 2 {
		t.Errorf("long text was not wrapped: %q", got)
	}
}

func TestChunkTarget(t *testing.T) {
	if got := (Options{}).chunkTarget(); got != chunk.DefaultTarget {
		t.Errorf("default target = %v, want %v", got, chunk.DefaultTarget)
	}
	if got := (Options{ChunkSeconds: 1}).chunkTarget(); got != chunk.DefaultTarget {
		t.Errorf("too-small target = %v, want the default", got)
	}
	if got := (Options{ChunkSeconds: 120}).chunkTarget(); got != 120 {
		t.Errorf("explicit target = %v, want 120", got)
	}
}

func TestRunValidatesInput(t *testing.T) {
	job := &Job{}
	if _, err := job.Run(t.Context(), Input{Path: "x"}, Options{Model: "m"}); err == nil {
		t.Error("Run accepted a job without ffmpeg")
	}
	job = &Job{Client: openrouter.New("k")}
	if _, err := job.Run(t.Context(), Input{Path: "x"}, Options{Model: ""}); err == nil {
		t.Error("Run accepted a job without a model")
	}
}

func TestSegmentsToCuesStretchesSpeedyAudio(t *testing.T) {
	// At 2× the model sees a clip half as long, so its timestamps must double
	// to land back on the original recording.
	result := &openrouter.TranscriptionResult{
		Segments: []openrouter.Segment{{Start: 1, End: 2, Text: "fast"}},
	}
	cues := segmentsToCues(result, chunk.Span{Start: 100, End: 400}, 2)
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if cues[0].Start != 102 || cues[0].End != 104 {
		t.Errorf("cue = %.2f..%.2f, want 102..104", cues[0].Start, cues[0].End)
	}
}

func TestRejectsAudioOnlyFiresForAudioComplaints(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"nemotron message", 400, `{"error":{"message":"Provider could not process the audio input (unsupported or malformed audio)","code":400}}`, true},
		{"unsupported media type", 415, `{"error":{"message":"audio format is not supported"}}`, true},
		{"empty audio", 400, `{"error":{"message":"audio is empty"}}`, true},
		{"bad model", 400, `{"error":{"message":"invalid model id"}}`, false},
		{"bad response format", 400, `{"error":{"message":"response_format is unsupported for this model"}}`, false},
		{"server error", 500, `{"error":{"message":"audio backend exploded"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &openrouter.APIError{StatusCode: tc.status, Message: messageOf(tc.body)}
			if got := rejectsAudio(err); got != tc.want {
				t.Errorf("rejectsAudio(%d %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestRejectsAudioIgnoresPlainErrors(t *testing.T) {
	if rejectsAudio(errors.New("audio was malformed")) {
		t.Error("a non-API error must not trigger a re-encode")
	}
}

func TestSpeedValueClampsToTheOfferedRange(t *testing.T) {
	cases := map[float64]float64{
		0: 1, 1: 1, 1.5: 1.5, 4: 4,
		-2: 1, 0.25: 1, 9: 1,
	}
	for in, want := range cases {
		if got := (Options{Speed: in}).speedValue(); got != want {
			t.Errorf("speedValue(%v) = %v, want %v", in, got, want)
		}
	}
}

// messageOf pulls the error message out of a fake API body, so the test cases
// read like the JSON the provider actually sends.
func messageOf(body string) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		panic(err)
	}
	return payload.Error.Message
}

// A remembered container goes first but must not shrink the ladder: a model
// that changes behaviour still gets the other encodings.
func TestOrderedFormatsPrefersTheRememberedContainer(t *testing.T) {
	defaults := ffmpeg.AudioFormats()
	if got := orderedFormats(""); len(got) != len(defaults) || got[0].Name != defaults[0].Name {
		t.Fatalf("no preference should keep the default order, got %v", names(got))
	}

	got := orderedFormats("pcm16")
	if len(got) != len(defaults) {
		t.Fatalf("preference dropped formats: %v", names(got))
	}
	if got[0].Name != "pcm16" {
		t.Errorf("first format is %q, want pcm16", got[0].Name)
	}
	// The order behind the preference is unchanged, so a rejection still walks
	// the ladder in the documented order.
	want := append([]string{"pcm16"}, names(ffmpeg.AudioFormats()[:len(defaults)-1])...)
	if strings.Join(names(got), ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", names(got), want)
	}

	if unknown := orderedFormats("nonsense"); names(unknown)[0] != defaults[0].Name {
		t.Errorf("unknown preference changed the order: %v", names(unknown))
	}
}

func names(formats []ffmpeg.AudioFormat) []string {
	out := make([]string, len(formats))
	for i, f := range formats {
		out[i] = f.Name
	}
	return out
}
