package openrouter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// TranscribeRequest describes one speech-to-text call.
type TranscribeRequest struct {
	// Model is the OpenRouter slug, e.g. "openai/whisper-large-v3".
	Model string
	// Audio is the raw (not base64) audio payload.
	Audio []byte
	// Format is one of wav, mp3, flac, m4a, ogg, webm, aac.
	Format string
	// Language is an optional ISO-639-1 hint such as "en".
	Language string
	// Temperature is optional; nil leaves the provider default in place.
	Temperature *float64
	// Verbose asks for segment timestamps (response_format=verbose_json).
	Verbose bool
	// ZDR restricts routing to zero-data-retention endpoints.
	ZDR bool
}

// Segment is a timestamped span of transcript returned by verbose_json.
type Segment struct {
	ID      int     `json:"id"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Text    string  `json:"text"`
	Speaker string  `json:"speaker,omitempty"`
}

// Usage reports what the request cost.
type Usage struct {
	Seconds      float64 `json:"seconds"`
	TotalTokens  int     `json:"total_tokens"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	Cost         float64 `json:"cost"`
}

// TranscriptionResult is the response body of the transcription endpoint.
type TranscriptionResult struct {
	Text       string    `json:"text"`
	Task       string    `json:"task"`
	Language   string    `json:"language"`
	Duration   float64   `json:"duration"`
	Confidence float64   `json:"confidence"`
	Segments   []Segment `json:"segments"`
	Usage      Usage     `json:"usage"`
}

type inputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
	URL    string `json:"url,omitempty"`
}

type providerPrefs struct {
	ZDR bool `json:"zdr,omitempty"`
}

type transcribeBody struct {
	Model                string         `json:"model"`
	InputAudio           inputAudio     `json:"input_audio"`
	Language             string         `json:"language,omitempty"`
	Temperature          *float64       `json:"temperature,omitempty"`
	ResponseFormat       string         `json:"response_format,omitempty"`
	TimestampGranularity []string       `json:"timestamp_granularities,omitempty"`
	Provider             *providerPrefs `json:"provider,omitempty"`
}

// Transcribe sends one audio payload to POST /audio/transcriptions.
func (c *Client) Transcribe(ctx context.Context, req TranscribeRequest) (*TranscriptionResult, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("no transcription model selected")
	}
	if len(req.Audio) == 0 {
		return nil, fmt.Errorf("audio payload is empty")
	}
	format := strings.ToLower(strings.TrimPrefix(req.Format, "."))
	if format == "" {
		format = "mp3"
	}

	body := transcribeBody{
		Model:       req.Model,
		InputAudio:  inputAudio{Data: base64.StdEncoding.EncodeToString(req.Audio), Format: format},
		Language:    req.Language,
		Temperature: req.Temperature,
	}
	if req.Verbose {
		body.ResponseFormat = "verbose_json"
		body.TimestampGranularity = []string{"segment"}
	}
	if req.ZDR {
		body.Provider = &providerPrefs{ZDR: true}
	}

	data, err := c.do(ctx, "POST", "/audio/transcriptions", body)
	if err != nil {
		return nil, err
	}

	var result TranscriptionResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode transcription response: %w", err)
	}
	if strings.TrimSpace(result.Text) == "" && len(result.Segments) == 0 {
		return nil, fmt.Errorf("the model returned an empty transcript (the audio may be silent or unsupported)")
	}
	return &result, nil
}

// ListTranscriptionModels returns only the models that produce transcripts.
func (c *Client) ListTranscriptionModels(ctx context.Context) ([]Model, error) {
	data, err := c.do(ctx, "GET", "/models?output_modalities=transcription", nil)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode model list: %w", err)
	}

	models := make([]Model, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID == "" {
			continue
		}
		// The query already filters server side; keep the client-side check so a
		// future API change cannot silently hand us chat models.
		if !m.SupportsTranscription() {
			continue
		}
		models = append(models, m)
	}
	SortModels(models)
	return models, nil
}

// VerifyKey checks that the API key is accepted. An empty response means the
// key is valid; a non-nil error carries the API's explanation.
func (c *Client) VerifyKey(ctx context.Context) error {
	_, err := c.do(ctx, "GET", "/key", nil)
	return err
}
