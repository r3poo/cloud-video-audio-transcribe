// Package subtitle converts timestamped transcript segments into SubRip text.
package subtitle

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Cue is one subtitle entry.
type Cue struct {
	Start float64
	End   float64
	Text  string
}

// Segment is a timestamped transcript span as returned by the API.
type Segment struct {
	Start float64
	End   float64
	Text  string
}

// DefaultLineWidth is the usual SubRip line length: two lines of 42 characters
// fit on screen without spilling.
const DefaultLineWidth = 42

// Timestamp renders seconds as HH:MM:SS,mmm (millisSeparator ',' for SubRip,
// '.' for WebVTT). Negative input is clamped to zero.
func Timestamp(seconds float64, millisSeparator byte) string {
	if math.IsNaN(seconds) || seconds < 0 {
		seconds = 0
	}
	total := int64(math.Round(seconds * 1000))
	h := total / 3_600_000
	m := (total % 3_600_000) / 60_000
	s := (total % 60_000) / 1000
	ms := total % 1000
	return fmt.Sprintf("%02d:%02d:%02d%c%03d", h, m, s, millisSeparator, ms)
}

// FromSegments shifts provider segments by offset so they are relative to the
// whole recording rather than the chunk they came from.
func FromSegments(segments []Segment, offset float64) []Cue {
	cues := make([]Cue, 0, len(segments))
	for _, s := range segments {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		start, end := s.Start+offset, s.End+offset
		if end <= start {
			continue
		}
		cues = append(cues, Cue{Start: start, End: end, Text: text})
	}
	return cues
}

// Estimate builds cues for a chunk whose provider did not return timestamps.
// Time is distributed by character count, which tracks speaking rate closely
// enough for a usable subtitle file.
func Estimate(text string, start, end float64, maxLine int) []Cue {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if maxLine <= 0 {
		maxLine = DefaultLineWidth
	}

	var lines []string
	var current strings.Builder
	for _, w := range words {
		switch {
		case current.Len() == 0:
			current.WriteString(w)
		case current.Len()+1+len(w) <= maxLine:
			current.WriteByte(' ')
			current.WriteString(w)
		default:
			lines = append(lines, current.String())
			current.Reset()
			current.WriteString(w)
		}
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}

	total := 0
	for _, l := range lines {
		total += len(l)
	}
	if total == 0 {
		return nil
	}

	duration := end - start
	if duration <= 0 {
		duration = 1
	}

	cues := make([]Cue, 0, len(lines))
	cumulative := 0
	cursor := start
	for i, line := range lines {
		cumulative += len(line)
		next := start + duration*float64(cumulative)/float64(total)
		if i == len(lines)-1 {
			next = end
		}
		if next <= cursor {
			next = cursor + 0.2
		}
		cues = append(cues, Cue{Start: cursor, End: next, Text: line})
		cursor = next
	}
	return cues
}

// Normalize sorts cues, drops empty text and guarantees a positive duration.
func Normalize(cues []Cue) []Cue {
	out := make([]Cue, 0, len(cues))
	for _, c := range cues {
		c.Text = strings.TrimSpace(c.Text)
		if c.Text == "" {
			continue
		}
		if c.Start < 0 {
			c.Start = 0
		}
		if c.End <= c.Start {
			c.End = c.Start + 0.5
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Start == out[j].Start {
			return out[i].End < out[j].End
		}
		return out[i].Start < out[j].Start
	})
	return out
}

// Wrap breaks text into lines no longer than maxLine characters, breaking on
// spaces where possible.
func Wrap(text string, maxLine int) []string {
	if maxLine <= 0 {
		maxLine = DefaultLineWidth
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	var lines []string
	var current strings.Builder
	for _, w := range words {
		if current.Len() == 0 {
			current.WriteString(w)
			continue
		}
		if current.Len()+1+len(w) <= maxLine {
			current.WriteByte(' ')
			current.WriteString(w)
			continue
		}
		lines = append(lines, current.String())
		current.Reset()
		current.WriteString(w)
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return lines
}

// FormatSRT renders cues as a SubRip document. Lines end with CRLF because
// Windows media players are the primary consumer.
func FormatSRT(cues []Cue) string {
	cues = Normalize(cues)

	var b strings.Builder
	for i, cue := range cues {
		lines := Wrap(cue.Text, DefaultLineWidth)
		if len(lines) == 0 {
			continue
		}
		fmt.Fprintf(&b, "%d\r\n%s --> %s\r\n", i+1, Timestamp(cue.Start, ','), Timestamp(cue.End, ','))
		for _, line := range lines {
			b.WriteString(line)
			b.WriteString("\r\n")
		}
		b.WriteString("\r\n")
	}
	return b.String()
}
