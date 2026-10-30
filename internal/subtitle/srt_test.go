package subtitle

import (
	"math"
	"strings"
	"testing"
)

func TestTimestamp(t *testing.T) {
	tests := []struct {
		seconds float64
		sep     byte
		want    string
	}{
		{0, ',', "00:00:00,000"},
		{1.5, ',', "00:00:01,500"},
		{3661.5, ',', "01:01:01,500"},
		{3725.25, '.', "01:02:05.250"},
		{-3, ',', "00:00:00,000"},
		{math.NaN(), ',', "00:00:00,000"},
		{59.9996, ',', "00:01:00,000"},
	}
	for _, tc := range tests {
		if got := Timestamp(tc.seconds, tc.sep); got != tc.want {
			t.Errorf("Timestamp(%v, %q) = %q, want %q", tc.seconds, tc.sep, got, tc.want)
		}
	}
}

func TestFromSegmentsShiftsByOffset(t *testing.T) {
	segments := []Segment{
		{Start: 0, End: 2.5, Text: "  hello "},
		{Start: 3, End: 4, Text: "world"},
	}
	cues := FromSegments(segments, 600)
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(cues))
	}
	if cues[0].Start != 600 || cues[0].End != 602.5 {
		t.Errorf("first cue = %v, want 600-602.5", cues[0])
	}
	if cues[0].Text != "hello" {
		t.Errorf("text = %q, want trimmed text", cues[0].Text)
	}
	if cues[1].Start != 603 {
		t.Errorf("second cue start = %v, want 603", cues[1].Start)
	}
}

func TestFromSegmentsDropsUnusable(t *testing.T) {
	segments := []Segment{
		{Start: 0, End: 1, Text: "   "},
		{Start: 2, End: 2, Text: "zero length"},
		{Start: 5, End: 4, Text: "backwards"},
		{Start: 6, End: 7, Text: "keep"},
	}
	cues := FromSegments(segments, 0)
	if len(cues) != 1 || cues[0].Text != "keep" {
		t.Fatalf("cues = %v, want only the usable segment", cues)
	}
}

func TestEstimateCoversSpanAndWraps(t *testing.T) {
	text := "one two three four five six seven eight nine ten eleven twelve thirteen fourteen"
	cues := Estimate(text, 100, 130, 20)
	if len(cues) < 3 {
		t.Fatalf("got %d cues, want the text split into several lines", len(cues))
	}
	if cues[0].Start != 100 {
		t.Errorf("first start = %v, want 100", cues[0].Start)
	}
	if last := cues[len(cues)-1]; last.End != 130 {
		t.Errorf("last end = %v, want 130", last.End)
	}
	for i, cue := range cues {
		if len(cue.Text) > 20 {
			t.Errorf("cue %d is %d chars, over the 20 char limit: %q", i, len(cue.Text), cue.Text)
		}
		if cue.End <= cue.Start {
			t.Errorf("cue %d has no duration: %v", i, cue)
		}
		if i > 0 && cue.Start < cues[i-1].Start {
			t.Errorf("cue %d starts before its predecessor: %v", i, cue.Start)
		}
	}
}

func TestEstimateHandlesEmptyText(t *testing.T) {
	if cues := Estimate("   ", 0, 10, 0); cues != nil {
		t.Fatalf("Estimate = %v, want nil", cues)
	}
}

func TestNormalize(t *testing.T) {
	cues := []Cue{
		{Start: 10, End: 12, Text: " second "},
		{Start: -1, End: 0.5, Text: "first"},
		{Start: 20, End: 20, Text: "zero length"},
		{Start: 30, End: 31, Text: "   "},
	}
	got := Normalize(cues)
	if len(got) != 3 {
		t.Fatalf("got %d cues, want 3: %v", len(got), got)
	}
	if got[0].Text != "first" || got[0].Start != 0 {
		t.Errorf("first = %v, want clamped start and sorted order", got[0])
	}
	if got[1].Start != 10 {
		t.Errorf("second = %v, want the 10s cue", got[1])
	}
	if got[2].End <= got[2].Start {
		t.Errorf("zero length cue was not repaired: %v", got[2])
	}
}

func TestFormatSRT(t *testing.T) {
	cues := []Cue{
		{Start: 0, End: 1.5, Text: "First line"},
		{Start: 2, End: 3, Text: "A sentence that is long enough to need wrapping at the usual width"},
	}
	out := FormatSRT(cues)

	if !strings.HasPrefix(out, "1\r\n00:00:00,000 --> 00:00:01,500\r\nFirst line\r\n\r\n") {
		t.Errorf("unexpected opening block:\n%q", out[:min(80, len(out))])
	}
	if !strings.Contains(out, "2\r\n00:00:02,000 --> 00:00:03,000\r\n") {
		t.Errorf("second cue header missing:\n%s", out)
	}
	if !strings.HasSuffix(out, "\r\n\r\n") {
		t.Errorf("document should end with a blank line:\n%q", out)
	}
	if strings.Contains(out, "\n\n\n") {
		t.Errorf("document has an empty block:\n%q", out)
	}
	for _, line := range strings.Split(out, "\r\n") {
		if len(line) > 42 && !strings.Contains(line, "-->") {
			t.Errorf("line longer than 42 chars: %q", line)
		}
	}
}

func TestFormatSRTEmpty(t *testing.T) {
	if got := FormatSRT(nil); got != "" {
		t.Errorf("FormatSRT(nil) = %q, want empty", got)
	}
}

func TestWrap(t *testing.T) {
	lines := Wrap("alpha beta gamma delta", 11)
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want 2", lines)
	}
	if lines[0] != "alpha beta" || lines[1] != "gamma delta" {
		t.Errorf("lines = %v", lines)
	}
	if got := Wrap("   ", 10); got != nil {
		t.Errorf("Wrap(blank) = %v, want nil", got)
	}
	if got := Wrap("averylongword", 4); len(got) != 1 || got[0] != "averylongword" {
		t.Errorf("a word longer than the width should stay on one line, got %v", got)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
