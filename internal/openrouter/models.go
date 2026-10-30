package openrouter

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Model is the subset of OpenRouter's model record that this program uses.
type Model struct {
	ID            string   `json:"id"`
	CanonicalSlug string   `json:"canonical_slug"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	ContextLength int      `json:"context_length"`
	Architecture  Arch     `json:"architecture"`
	Pricing       Pricing  `json:"pricing"`
	SupportedArgs []string `json:"supported_parameters"`
}

// Arch describes the modalities a model accepts and produces.
type Arch struct {
	Modality         string   `json:"modality"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
}

// Pricing holds the USD-per-token strings OpenRouter publishes. They are
// strings (not numbers) in the API payload, so they are kept as such.
type Pricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

// SupportsTranscription reports whether the model advertises transcription
// output.
func (m Model) SupportsTranscription() bool {
	for _, o := range m.Architecture.OutputModalities {
		if strings.EqualFold(o, "transcription") {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(m.Architecture.Modality), "audio->transcription")
}

// AcceptsAudio reports whether the model lists audio among its input
// modalities. Older records leave the slice empty, in which case the coarse
// modality string ("audio->transcription") is consulted instead.
func (m Model) AcceptsAudio() bool {
	if len(m.Architecture.InputModalities) == 0 {
		return strings.Contains(strings.ToLower(m.Architecture.Modality), "audio")
	}
	for _, in := range m.Architecture.InputModalities {
		if strings.EqualFold(in, "audio") {
			return true
		}
	}
	return false
}

// InputLabel renders the accepted input modalities, e.g. "audio, text".
func (m Model) InputLabel() string {
	if len(m.Architecture.InputModalities) == 0 {
		if m.Architecture.Modality != "" {
			return m.Architecture.Modality
		}
		return "input types unknown"
	}
	return "input: " + strings.Join(m.Architecture.InputModalities, ", ")
}

// InputPricePerMillion converts the per-token prompt price into a
// per-million-token figure, which is easier to read in a UI.
func (m Model) InputPricePerMillion() (float64, bool) {
	if m.Pricing.Prompt == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(m.Pricing.Prompt, 64)
	if err != nil {
		return 0, false
	}
	return f * 1_000_000, true
}

// PriceLabel renders the input price for display, e.g. "$0.0075 / 1M tokens".
func (m Model) PriceLabel() string {
	p, ok := m.InputPricePerMillion()
	switch {
	case !ok:
		return "price unavailable"
	case p == 0:
		return "free"
	case p < 0.01:
		return fmt.Sprintf("$%.4f / 1M input tokens", p)
	case p < 1:
		return fmt.Sprintf("$%.3f / 1M input tokens", p)
	default:
		return fmt.Sprintf("$%.2f / 1M input tokens", p)
	}
}

// DisplayName is what the model picker shows: the human name plus the slug,
// because two providers can publish models with similar names.
func (m Model) DisplayName() string {
	name := m.Name
	if name == "" {
		name = m.ID
	}
	return fmt.Sprintf("%s  (%s)", name, m.ID)
}

// SortModels orders models by display name, keeping the list stable for the UI.
func SortModels(models []Model) {
	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i].DisplayName(), models[j].DisplayName()
		return strings.ToLower(a) < strings.ToLower(b)
	})
}

// pickDefaultIndex returns the index of a sensible default model: Whisper
// large-v3 when available, otherwise the first entry.
func pickDefaultIndex(models []Model) int {
	for i, m := range models {
		if m.ID == "openai/whisper-large-v3" {
			return i
		}
	}
	for i, m := range models {
		if strings.Contains(strings.ToLower(m.ID), "whisper") {
			return i
		}
	}
	return 0
}

// DefaultModelIndex exposes pickDefaultIndex for the UI layer.
func DefaultModelIndex(models []Model) int {
	if len(models) == 0 {
		return -1
	}
	return pickDefaultIndex(models)
}
