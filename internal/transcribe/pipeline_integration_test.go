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
		if atomic.AddInt32(&requests, 1) == 2 {
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
