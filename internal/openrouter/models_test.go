package openrouter

import (
	"strings"
	"testing"
)

func TestSupportsTranscription(t *testing.T) {
	tests := []struct {
		name  string
		model Model
		want  bool
	}{
		{
			name:  "output modality",
			model: Model{ID: "a", Architecture: Arch{OutputModalities: []string{"transcription"}}},
			want:  true,
		},
		{
			name:  "modality string",
			model: Model{ID: "b", Architecture: Arch{Modality: "audio->transcription"}},
			want:  true,
		},
		{
			name:  "chat model",
			model: Model{ID: "c", Architecture: Arch{Modality: "text->text", OutputModalities: []string{"text"}}},
			want:  false,
		},
		{
			name:  "empty record",
			model: Model{ID: "d"},
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.model.SupportsTranscription(); got != tc.want {
				t.Errorf("SupportsTranscription() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAcceptsAudio(t *testing.T) {
	withInputs := Model{Architecture: Arch{InputModalities: []string{"audio", "text"}}}
	if !withInputs.AcceptsAudio() {
		t.Error("model listing audio input was rejected")
	}
	textOnly := Model{Architecture: Arch{InputModalities: []string{"text"}}}
	if textOnly.AcceptsAudio() {
		t.Error("text-only model was accepted")
	}
	// Records without input modalities fall back to the coarse string.
	legacy := Model{Architecture: Arch{Modality: "audio->transcription"}}
	if !legacy.AcceptsAudio() {
		t.Error("legacy modality record was rejected")
	}
	if got := legacy.InputLabel(); got != "audio->transcription" {
		t.Errorf("InputLabel = %q", got)
	}
	if got := withInputs.InputLabel(); got != "input: audio, text" {
		t.Errorf("InputLabel = %q", got)
	}
}

func TestPriceLabel(t *testing.T) {
	tests := []struct {
		prompt string
		want   string
	}{
		{"", "price unavailable"},
		{"0", "free"},
		{"0.0000001", "$0.100 / 1M input tokens"},
		{"0.0000005", "$0.500 / 1M input tokens"},
		{"0.000005", "$5.00 / 1M input tokens"},
		{"not-a-number", "price unavailable"},
	}
	for _, tc := range tests {
		m := Model{Pricing: Pricing{Prompt: tc.prompt}}
		if got := m.PriceLabel(); got != tc.want {
			t.Errorf("PriceLabel(prompt=%q) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
}

func TestDisplayName(t *testing.T) {
	m := Model{ID: "openai/whisper-1", Name: "Whisper 1"}
	if got := m.DisplayName(); got != "Whisper 1  (openai/whisper-1)" {
		t.Errorf("DisplayName = %q", got)
	}
	unnamed := Model{ID: "vendor/stt"}
	if got := unnamed.DisplayName(); !strings.Contains(got, "vendor/stt") {
		t.Errorf("DisplayName = %q, want the id when the name is missing", got)
	}
}

func TestSortModels(t *testing.T) {
	models := []Model{
		{ID: "b", Name: "Beta"},
		{ID: "a", Name: "alpha"},
		{ID: "c", Name: "Gamma"},
	}
	SortModels(models)
	got := []string{models[0].Name, models[1].Name, models[2].Name}
	want := []string{"alpha", "Beta", "Gamma"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want case-insensitive alphabetical %v", got, want)
		}
	}
}

func TestDefaultModelIndex(t *testing.T) {
	if got := DefaultModelIndex(nil); got != -1 {
		t.Errorf("DefaultModelIndex(nil) = %d, want -1", got)
	}

	withLarge := []Model{{ID: "x/whisper-1"}, {ID: "openai/whisper-large-v3"}}
	if got := DefaultModelIndex(withLarge); got != 1 {
		t.Errorf("DefaultModelIndex = %d, want the whisper-large-v3 entry", got)
	}

	whisper := []Model{{ID: "vendor/other"}, {ID: "vendor/Whisper-1"}}
	if got := DefaultModelIndex(whisper); got != 1 {
		t.Errorf("DefaultModelIndex = %d, want any whisper model", got)
	}

	plain := []Model{{ID: "vendor/other"}, {ID: "vendor/more"}}
	if got := DefaultModelIndex(plain); got != 0 {
		t.Errorf("DefaultModelIndex = %d, want 0", got)
	}
}

func TestAPIErrorHints(t *testing.T) {
	tests := []struct {
		status  int
		retries bool
		hint    string
	}{
		{401, false, "OPENROUTER_API_KEY"},
		{402, false, "credits"},
		{404, false, "model"},
		{413, false, "TRANSCRIBE_CHUNK_SECONDS"},
		{429, true, "retry"},
		{500, true, "retrying"},
		{400, false, ""},
	}
	for _, tc := range tests {
		err := &APIError{StatusCode: tc.status, Message: "boom"}
		if got := err.Retryable(); got != tc.retries {
			t.Errorf("status %d: Retryable = %v, want %v", tc.status, got, tc.retries)
		}
		if !strings.Contains(strings.ToLower(err.HumanHint()), strings.ToLower(tc.hint)) {
			t.Errorf("status %d: HumanHint = %q, want it to mention %q", tc.status, err.HumanHint(), tc.hint)
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("status %d: Error() lost the message: %q", tc.status, err.Error())
		}
	}
}

func TestRetryDelayHonoursRetryAfter(t *testing.T) {
	err := &APIError{StatusCode: 429, RetryAfter: 2 * 1e9} // 2s
	if got := err.RetryDelay(1); got != 2*1e9 {
		t.Errorf("RetryDelay = %v, want the Retry-After value", got)
	}
	backoff := &APIError{StatusCode: 500}
	if got := backoff.RetryDelay(1); got != 2*1e9 {
		t.Errorf("RetryDelay(1) = %v, want 2s", got)
	}
	if got := backoff.RetryDelay(20); got > 30*1e9 {
		t.Errorf("RetryDelay(20) = %v, want it capped at 30s", got)
	}
}
