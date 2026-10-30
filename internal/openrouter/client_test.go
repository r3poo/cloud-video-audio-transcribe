package openrouter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient wires a client to srv and removes the backoff sleeps.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := New("test-key")
	c.BaseURL = srv.URL
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestTranscribeSendsDocumentedShape(t *testing.T) {
	audio := []byte("fake-mp3-bytes")

	var gotPath, gotAuth, gotMethod string
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"text": "hello world",
			"language": "en",
			"duration": 2.5,
			"segments": [{"id": 0, "start": 0, "end": 2.5, "text": "hello world"}],
			"usage": {"seconds": 2.5, "cost": 0.00003}
		}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	result, err := client.Transcribe(context.Background(), TranscribeRequest{
		Model:    "openai/whisper-large-v3",
		Audio:    audio,
		Format:   ".MP3",
		Language: "en",
		Verbose:  true,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/audio/transcriptions" {
		t.Errorf("request = %s %s, want POST /audio/transcriptions", gotMethod, gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if body["model"] != "openai/whisper-large-v3" {
		t.Errorf("model = %v", body["model"])
	}
	if body["language"] != "en" {
		t.Errorf("language = %v", body["language"])
	}
	if body["response_format"] != "verbose_json" {
		t.Errorf("response_format = %v, want verbose_json", body["response_format"])
	}
	granularities, ok := body["timestamp_granularities"].([]any)
	if !ok || len(granularities) != 1 || granularities[0] != "segment" {
		t.Errorf("timestamp_granularities = %v", body["timestamp_granularities"])
	}

	inputAudio, ok := body["input_audio"].(map[string]any)
	if !ok {
		t.Fatalf("input_audio missing or not an object: %v", body["input_audio"])
	}
	if inputAudio["format"] != "mp3" {
		t.Errorf("input_audio.format = %v, want the normalised extension", inputAudio["format"])
	}
	decoded, err := base64.StdEncoding.DecodeString(inputAudio["data"].(string))
	if err != nil {
		t.Fatalf("input_audio.data is not base64: %v", err)
	}
	if string(decoded) != string(audio) {
		t.Errorf("audio round trip = %q", decoded)
	}

	if result.Text != "hello world" {
		t.Errorf("text = %q", result.Text)
	}
	if len(result.Segments) != 1 || result.Segments[0].End != 2.5 {
		t.Errorf("segments = %v", result.Segments)
	}
	if result.Usage.Cost != 0.00003 {
		t.Errorf("cost = %v", result.Usage.Cost)
	}
}

func TestTranscribeOmitsOptionalFields(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		io.WriteString(w, `{"text":"ok"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	if _, err := client.Transcribe(context.Background(), TranscribeRequest{
		Model: "m", Audio: []byte("a"), Format: "wav",
	}); err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	for _, field := range []string{"language", "response_format", "timestamp_granularities", "provider"} {
		if _, ok := body[field]; ok {
			t.Errorf("%q was sent although it was not requested: %v", field, body[field])
		}
	}
}

func TestTranscribeValidatesInput(t *testing.T) {
	client := New("k")
	if _, err := client.Transcribe(context.Background(), TranscribeRequest{Audio: []byte("a")}); err == nil {
		t.Error("accepted a request without a model")
	}
	if _, err := client.Transcribe(context.Background(), TranscribeRequest{Model: "m"}); err == nil {
		t.Error("accepted a request without audio")
	}
}

func TestTranscribeRejectsEmptyTranscript(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"text":"","segments":[]}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, err := client.Transcribe(context.Background(), TranscribeRequest{Model: "m", Audio: []byte("a")})
	if err == nil || !strings.Contains(err.Error(), "empty transcript") {
		t.Fatalf("err = %v, want an empty transcript error", err)
	}
}

func TestTranscribeSurfacesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"No auth credentials found","code":401}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	_, err := client.Transcribe(context.Background(), TranscribeRequest{Model: "m", Audio: []byte("a")})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *APIError", err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Message, "No auth credentials") {
		t.Errorf("message = %q", apiErr.Message)
	}
}

func TestTranscribeRetriesTransientFailure(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		io.WriteString(w, `{"text":"second time lucky"}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	result, err := client.Transcribe(context.Background(), TranscribeRequest{Model: "m", Audio: []byte("a")})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if result.Text != "second time lucky" {
		t.Errorf("text = %q", result.Text)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

func TestTranscribeDoesNotRetryClientError(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad audio"}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	if _, err := client.Transcribe(context.Background(), TranscribeRequest{Model: "m", Audio: []byte("a")}); err == nil {
		t.Fatal("expected an error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want a 4xx to be tried once", attempts)
	}
}

func TestListTranscriptionModelsFiltersAndSorts(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		io.WriteString(w, `{"data":[
			{"id":"vendor/zeta","name":"Zeta STT","architecture":{"output_modalities":["transcription"],"input_modalities":["audio"]},"pricing":{"prompt":"0.0000005"}},
			{"id":"vendor/chat","name":"Chat","architecture":{"modality":"text->text","output_modalities":["text"]}},
			{"id":"openai/whisper-large-v3","name":"Whisper Large v3","architecture":{"modality":"audio->transcription","output_modalities":["transcription"]}}
		]}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv)
	models, err := client.ListTranscriptionModels(context.Background())
	if err != nil {
		t.Fatalf("ListTranscriptionModels: %v", err)
	}
	if !strings.Contains(query, "output_modalities=transcription") {
		t.Errorf("query = %q, want the server-side transcription filter", query)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want the chat model filtered out: %v", len(models), models)
	}
	if models[0].ID != "openai/whisper-large-v3" || models[1].ID != "vendor/zeta" {
		t.Errorf("order = %q, %q, want alphabetical by display name", models[0].ID, models[1].ID)
	}
}

func TestVerifyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key" {
			t.Errorf("path = %q, want /key", r.URL.Path)
		}
		io.WriteString(w, `{"data":{"label":"test"}}`)
	}))
	defer srv.Close()

	if err := newTestClient(t, srv).VerifyKey(context.Background()); err != nil {
		t.Errorf("VerifyKey: %v", err)
	}
}
