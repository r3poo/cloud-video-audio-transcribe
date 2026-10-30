// Package chunk decides how to split a long recording into API-sized pieces.
//
// OpenRouter times out an upstream transcription request after roughly 60
// seconds of processing, and providers cap inline audio payloads at 25 MB, so
// anything longer than a few minutes has to be sent in pieces and stitched back
// together.
package chunk

import (
	"fmt"
	"math"
	"sort"
)

const (
	// MinTarget is the smallest chunk length worth a request. Anything shorter
	// would multiply request counts without reducing timeouts.
	MinTarget = 30.0

	// DefaultTarget is ten minutes. At 16 kHz mono MP3 (64 kbps) that is about
	// 4.8 MB per chunk, comfortably under the 25 MB inline ceiling.
	DefaultTarget = 600.0
)

// Span is a half-open interval [Start, End) in seconds from the start of the
// source media.
type Span struct {
	Start float64
	End   float64
}

// Duration is the length of the span in seconds.
func (s Span) Duration() float64 { return s.End - s.Start }

// String renders the span as "0:00:00-0:10:00".
func (s Span) String() string {
	return fmt.Sprintf("%s-%s", clock(s.Start), clock(s.End))
}

func clock(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}
	total := int(seconds)
	return fmt.Sprintf("%d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// Plan splits a recording into spans of roughly equal length, snapping
// boundaries to nearby silence timestamps when that is possible. Snapping keeps
// words intact: cutting mid-word makes the model start a chunk with a mangled
// fragment.
//
// cuts holds candidate boundaries in seconds (the end of detected silences).
// It may be nil, in which case boundaries fall on the even grid.
func Plan(duration, target float64, cuts []float64) []Span {
	if duration <= 0 {
		return nil
	}
	if target < MinTarget {
		target = MinTarget
	}
	if duration <= target {
		return []Span{{Start: 0, End: duration}}
	}

	// Divide the recording evenly instead of filling fixed-size chunks: a
	// fixed grid leaves a stub or a double-length chunk at the end, and a
	// double-length chunk is the one most likely to hit the provider timeout.
	ratio := duration / target
	count := int(math.Ceil(ratio))
	// A tail shorter than 15% of a chunk is not worth its own request; fold it
	// into the last full chunk.
	if frac := ratio - math.Floor(ratio); frac > 0 && frac < 0.15 && count > 1 {
		count--
	}
	step := duration / float64(count)

	// Search this far either side of a grid boundary for silence.
	window := step / 4
	if window < 10 {
		window = 10
	}
	if window > 120 {
		window = 120
	}

	sorted := append([]float64(nil), cuts...)
	sort.Float64s(sorted)

	spans := make([]Span, 0, count)
	start := 0.0
	for i := 1; i < count; i++ {
		boundary := float64(i) * step
		// Reject snaps that would produce a chunk too short to be worth a
		// request, or that would swallow the whole tail.
		if cut, ok := nearest(sorted, boundary, window); ok && cut > start+MinTarget && cut < duration-MinTarget {
			boundary = cut
		}
		spans = append(spans, Span{Start: start, End: boundary})
		start = boundary
	}
	return append(spans, Span{Start: start, End: duration})
}

// nearest returns the candidate closest to want, provided it lies within
// tolerance.
func nearest(sorted []float64, want, tolerance float64) (float64, bool) {
	if len(sorted) == 0 {
		return 0, false
	}
	idx := sort.SearchFloat64s(sorted, want)
	best, bestDist := 0.0, tolerance
	found := false
	for _, i := range []int{idx - 1, idx} {
		if i < 0 || i >= len(sorted) {
			continue
		}
		dist := sorted[i] - want
		if dist < 0 {
			dist = -dist
		}
		if dist <= bestDist {
			best, bestDist, found = sorted[i], dist, true
		}
	}
	return best, found
}
