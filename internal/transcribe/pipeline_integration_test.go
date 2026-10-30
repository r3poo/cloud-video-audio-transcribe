package transcribe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloudtranscribe/internal/executil"
	"cloudtranscribe/internal/ffmpeg"
	"cloudtranscribe/internal/openrouter"
)

// TestJobEndToEnd wires a real ffmpeg to a stand-in OpenRouter server, so the
// split → extract → upload → merge path is covered without an API key.
func TestJobEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ffmpeg integration test in short mode")
	}
	ctx := context.Background()

	install, ok := ffmpeg.Resolve(ctx, "")
	if !ok {
		t.Skip("ffmpeg is not installed; skipping the end-to-end test")
	}

	dir := t.TempDir()
	sample := filepath.Join(dir, "lecture.wav")
	// 70 seconds of tone: long enough for 30s chunks to split three ways.
	cmd := exec.CommandContext(ctx, install.FFmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=70",
		"-y", sample)
	executil.Hide(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the sample file: %v\n%s", err, out)
	}

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)

		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model      string `json:"model"`
			InputAudio struct {
				Data   string `json:"data"`
				Format string `json:"format"`
			} `json:"input_audio"`
			ResponseFormat string `json:"response_format"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request %d is not JSON: %v", n, err)
			return
		}
		if body.Model != "test/whisper" {
			t.Errorf("model = %q", body.Model)
		}
		if body.InputAudio.Data == "" || body.InputAudio.Format == "" {
			t.Errorf("request %d carried no audio", n)
		}
		if body.ResponseFormat != "verbose_json" {
			t.Errorf("response_format = %q, want verbose_json", body.ResponseFormat)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"text":     "part text",
			"language": "en",
			"segments": []map[string]any{
				{"id": 0, "start": 1.0, "end": 3.5, "text": "line one"},
				{"id": 1, "start": 4.0, "end": 6.0, "text": "line two"},
			},
			"usage": map[string]any{"cost": 0.00001},
		})
	}))
	defer srv.Close()

	client := openrouter.New("test-key")
	client.BaseURL = srv.URL

	var stages []string
	job := &Job{
		FFmpeg: install,
		Client: client,
		Progress: func(p Progress) {
			if p.Stage != "" && (len(stages) == 0 || stages[len(stages)-1] != p.Stage) {
				stages = append(stages, p.Stage)
			}
		},
	}

	result, err := job.Run(ctx, Input{Path: sample}, Options{
		Model:        "test/whisper",
		ChunkSeconds: 30,
		Language:     "en",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if requests != 3 || result.Spans != 3 {
		t.Errorf("requests = %d, spans = %d, want 3 each", requests, result.Spans)
	}
	if result.Duration < 69*time.Second || result.Duration > 71*time.Second {
		t.Errorf("duration = %v, want about 70s", result.Duration)
	}
	if len(result.Failed) != 0 {
		t.Errorf("failed spans = %v", result.Failed)
	}
	if got := result.CostUSD; got < 0.000029 || got > 0.000031 {
		t.Errorf("cost = %v, want the three reported costs summed", got)
	}
	if result.Segments != 6 {
		t.Errorf("segments = %d, want two cues per chunk", result.Segments)
	}

	srt, err := os.ReadFile(result.SRTPath)
	if err != nil {
		t.Fatalf("reading the transcript: %v", err)
	}
	text := string(srt)
	// Chunks are split evenly (70/3 s each), so the second chunk's first cue
	// lands at 23.33 + 1 and the third at 46.67 + 1.
	for _, want := range []string{"00:00:01,000", "00:00:24,333", "00:00:47,667", "\r\n4\r\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript is missing %q:\n%s", want, text)
		}
	}
	if !strings.HasSuffix(result.TextPath, "lecture.txt") {
		t.Errorf("text path = %q", result.TextPath)
	}
	txt, err := os.ReadFile(result.TextPath)
	if err != nil {
		t.Fatalf("reading the text transcript: %v", err)
	}
	if strings.Count(string(txt), "part text") != 3 {
		t.Errorf("text transcript should hold one paragraph per chunk:\n%s", txt)
	}

	wantStages := []string{"probing", "planning", "downloading audio", "uploading", "writing"}
	for _, want := range wantStages {
		found := false
		for _, got := range stages {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("progress never reported stage %q (saw %v)", want, stages)
		}
	}
}

// TestJobKeepsGoingAfterOneFailedSpan checks that a single bad chunk does not
// throw away the rest of a long transcript.
func TestJobKeepsGoingAfterOneFailedSpan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ffmpeg integration test in short mode")
	}
	ctx := context.Background()

	install, ok := ffmpeg.Resolve(ctx, "")
	if !ok {
		t.Skip("ffmpeg is not installed; skipping the end-to-end test")
	}

	dir := t.TempDir()
	sample := filepath.Join(dir, "call.wav")
	cmd := exec.CommandContext(ctx, install.FFmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=70",
		"-y", sample)
	executil.Hide(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the sample file: %v\n%s", err, out)
	}

	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One span is unusable. Its first attempt is request 2 and the plain
		// retry is request 3; both must fail, which is what makes the span
		// count as failed rather than silently degraded.
		if n := atomic.AddInt32(&requests, 1); n == 2 || n == 3 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"bad audio for this chunk"}}`)
			return
		}
		io.WriteString(w, `{"text":"ok","segments":[{"start":0,"end":2,"text":"ok"}]}`)
	}))
	defer srv.Close()

	client := openrouter.New("test-key")
	client.BaseURL = srv.URL
	client.MaxAttempts = 1

	result, err := (&Job{FFmpeg: install, Client: client}).Run(ctx, Input{Path: sample}, Options{
		Model:        "test/whisper",
		ChunkSeconds: 30,
	})
	if err != nil {
		t.Fatalf("Run should survive one failed chunk: %v", err)
	}
	if len(result.Failed) != 1 {
		t.Fatalf("failed = %v, want exactly one span", result.Failed)
	}
	if result.Segments != 2 {
		t.Errorf("segments = %d, want the two successful chunks", result.Segments)
	}
	if _, err := os.Stat(result.SRTPath); err != nil {
		t.Errorf("transcript was not written: %v", err)
	}
}

// TestJobRetriesRejectedEncoding reproduces the failure a Riva-backed model
// reports — "unsupported or malformed audio" for the first container — and
// checks that the next encoding is used without losing the chunk. It also runs
// at 2x, so the timestamps the model reports for the shortened clip must be
// stretched back onto the original timeline.
func TestJobRetriesRejectedEncoding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ffmpeg integration test in short mode")
	}
	ctx := context.Background()

	install, ok := ffmpeg.Resolve(ctx, "")
	if !ok {
		t.Skip("ffmpeg is not installed; skipping the end-to-end test")
	}

	dir := t.TempDir()
	sample := filepath.Join(dir, "talk.wav")
	cmd := exec.CommandContext(ctx, install.FFmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "sine=frequency=300:duration=20",
		"-y", sample)
	executil.Hide(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the sample file: %v\n%s", err, out)
	}

	var (
		requests int32
		mu       sync.Mutex
		formats  []string
		shapes   []string
		payloads []int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			InputAudio struct {
				Data   string `json:"data"`
				Format string `json:"format"`
			} `json:"input_audio"`
			ResponseFormat string `json:"response_format"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request is not JSON: %v", err)
			return
		}
		mu.Lock()
		formats = append(formats, body.InputAudio.Format)
		shapes = append(shapes, body.ResponseFormat)
		payloads = append(payloads, len(body.InputAudio.Data))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if body.InputAudio.Format == "wav" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Provider could not process the audio input (unsupported or malformed audio)","code":400}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"text": "sped up",
			"segments": []map[string]any{
				{"id": 0, "start": 1.0, "end": 2.0, "text": "at double speed"},
			},
			"usage": map[string]any{"cost": 0.00001},
		})
	}))
	defer srv.Close()

	client := openrouter.New("test-key")
	client.BaseURL = srv.URL

	job := &Job{FFmpeg: install, Client: client}
	result, err := job.Run(ctx, Input{Path: sample}, Options{
		Model: "nvidia/nemotron-test",
		Speed: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// The refused container is retried once in the plain shape before the
	// ladder moves on, so the second wav upload is expected: it is the same
	// bytes with the same rejection.
	wantFormats := []string{"wav", "wav", "mp3"}
	if strings.Join(formats, ",") != strings.Join(wantFormats, ",") {
		t.Fatalf("formats tried = %v, want %v", formats, wantFormats)
	}
	// The first request asks for timestamps; the plain retry omits
	// response_format, which means the default json shape.
	if shapes[0] != "verbose_json" {
		t.Errorf("first request shape = %q, want verbose_json", shapes[0])
	}
	if shapes[len(shapes)-1] != "" {
		t.Errorf("last request shape = %q, want the plain default", shapes[len(shapes)-1])
	}
	if result.AudioFormat != "mp3" {
		t.Errorf("result.AudioFormat = %q, want the accepted container", result.AudioFormat)
	}
	if !result.TimestampsEstimated {
		t.Error("TimestampsEstimated should be set after the timestamped shape was refused")
	}
	if len(result.Failed) != 0 {
		t.Errorf("the chunk failed despite a working fallback: %v", result.Failed)
	}

	// The model saw 2 seconds of audio and reported 1s..2s, which is 2s..4s of
	// the recording.
	srt, err := os.ReadFile(result.SRTPath)
	if err != nil {
		t.Fatalf("reading the transcript: %v", err)
	}
	if !strings.Contains(string(srt), "00:00:02,000 --> 00:00:04,000") {
		t.Errorf("timestamps were not stretched back to the original timeline:\n%s", srt)
	}
}

// TestJobDropsTimestampsBeforeChangingEncoding covers the other reading of the
// same generic 400. A route that cannot produce timestamped output answers
// every container with "could not process the audio input"; re-encoding to mp3,
// flac and pcm16 would then burn four uploads to learn what one plain retry
// shows for free.
func TestJobDropsTimestampsBeforeChangingEncoding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ffmpeg integration test in short mode")
	}
	ctx := context.Background()

	install, ok := ffmpeg.Resolve(ctx, "")
	if !ok {
		t.Skip("ffmpeg is not installed; skipping the end-to-end test")
	}

	dir := t.TempDir()
	sample := filepath.Join(dir, "talk.wav")
	cmd := exec.CommandContext(ctx, install.FFmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "sine=frequency=300:duration=8",
		"-y", sample)
	executil.Hide(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the sample file: %v\n%s", err, out)
	}

	var (
		mu      sync.Mutex
		formats []string
		shapes  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			InputAudio struct {
				Format string `json:"format"`
			} `json:"input_audio"`
			ResponseFormat      string   `json:"response_format"`
			TimestampGranularit []string `json:"timestamp_granularities"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request is not JSON: %v", err)
			return
		}
		mu.Lock()
		formats = append(formats, body.InputAudio.Format)
		shapes = append(shapes, body.ResponseFormat)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// Exactly the message NVIDIA's route produced for every container.
		if len(body.TimestampGranularit) > 0 || body.ResponseFormat == "verbose_json" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"Provider could not process the audio input (unsupported or malformed audio)","code":400}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"text":  "no timestamps here",
			"usage": map[string]any{"cost": 0.00001},
		})
	}))
	defer srv.Close()

	client := openrouter.New("test-key")
	client.BaseURL = srv.URL

	job := &Job{FFmpeg: install, Client: client}
	result, err := job.Run(ctx, Input{Path: sample}, Options{Model: "nvidia/nemotron-test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantFormats := []string{"wav", "wav"}
	if strings.Join(formats, ",") != strings.Join(wantFormats, ",") {
		t.Fatalf("formats tried = %v, want %v (no re-encoding before the plain retry)", formats, wantFormats)
	}
	if result.AudioFormat != "wav" {
		t.Errorf("result.AudioFormat = %q, want wav", result.AudioFormat)
	}
	if !result.TimestampsEstimated {
		t.Error("TimestampsEstimated should be set when the model gave no segment times")
	}
	if result.Segments == 0 {
		t.Error("cues should still be written from the plain transcript")
	}
	if len(result.Failed) != 0 {
		t.Errorf("the chunk failed despite a working plain request: %v", result.Failed)
	}
}

// TestJobSplitsWhenTheGatewaySaysTooLarge covers the payload guard being wrong:
// a span that looked small enough is refused with 413. The span is halved and
// the halves are uploaded, instead of the whole span being written off.
func TestJobSplitsWhenTheGatewaySaysTooLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ffmpeg integration test in short mode")
	}
	ctx := context.Background()

	install, ok := ffmpeg.Resolve(ctx, "")
	if !ok {
		t.Skip("ffmpeg is not installed; skipping the end-to-end test")
	}

	dir := t.TempDir()
	sample := filepath.Join(dir, "long.wav")
	cmd := exec.CommandContext(ctx, install.FFmpeg,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-f", "lavfi", "-i", "sine=frequency=220:duration=90",
		"-y", sample)
	executil.Hide(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the sample file: %v\n%s", err, out)
	}

	// 90s of audio fits under the guard, so the first send is a whole span.
	// Only the first request is refused; the two halves are accepted.
	var (
		requests int32
		mu       sync.Mutex
		payloads []int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requests, 1)
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			InputAudio struct {
				Data string `json:"data"`
			} `json:"input_audio"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request is not JSON: %v", err)
			return
		}
		mu.Lock()
		payloads = append(payloads, len(body.InputAudio.Data))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			io.WriteString(w, `{"error":{"message":"Payload too large","code":413}}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"text":     "half a recording",
			"segments": []map[string]any{{"id": 0, "start": 0, "end": 3, "text": "half a recording"}},
		})
	}))
	defer srv.Close()

	client := openrouter.New("test-key")
	client.BaseURL = srv.URL
	client.MaxAttempts = 1

	result, err := (&Job{FFmpeg: install, Client: client}).Run(ctx, Input{Path: sample}, Options{Model: "test/whisper"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Failed) != 0 {
		t.Errorf("the span was written off instead of split: %v", result.Failed)
	}
	// Spans counts the planned spans, so the split is only visible in the
	// request log below.
	if result.Segments == 0 {
		t.Error("the halves produced no cues")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 3 {
		t.Fatalf("requests = %d, want the refused upload plus two halves", len(payloads))
	}
	// Each half must carry less than the refused whole, or the split bought
	// nothing.
	if payloads[1] >= payloads[0] || payloads[2] >= payloads[0] {
		t.Errorf("halves were not smaller than the refused span: %v", payloads)
	}
}
