package chunk

import (
	"math"
	"testing"
)

func TestPlanRejectsEmptyInput(t *testing.T) {
	if spans := Plan(0, DefaultTarget, nil); spans != nil {
		t.Fatalf("Plan(0) = %v, want nil", spans)
	}
	if spans := Plan(-5, DefaultTarget, nil); spans != nil {
		t.Fatalf("Plan(-5) = %v, want nil", spans)
	}
}

func TestPlanShortRecordingIsOneSpan(t *testing.T) {
	spans := Plan(120, DefaultTarget, nil)
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1: %v", len(spans), spans)
	}
	if spans[0].Start != 0 || spans[0].End != 120 {
		t.Errorf("span = %v, want 0-120", spans[0])
	}
}

func TestPlanSplitsLongRecording(t *testing.T) {
	spans := Plan(1500, 600, nil)
	if len(spans) != 3 {
		t.Fatalf("got %d spans, want 3: %v", len(spans), spans)
	}
	assertCovers(t, spans, 1500)
}

func TestPlanAbsorbsTrailingSliver(t *testing.T) {
	spans := Plan(610, 600, nil)
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want the 10s tail folded in: %v", len(spans), spans)
	}
	if spans[0].End != 610 {
		t.Errorf("end = %v, want 610", spans[0].End)
	}
}

func TestPlanSnapsToSilence(t *testing.T) {
	// A silence ends 2:00 before the grid point, inside the +/-150s window.
	spans := Plan(1000, 600, []float64{480})
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %v", len(spans), spans)
	}
	if spans[0].End != 480 {
		t.Errorf("first boundary = %v, want 480", spans[0].End)
	}
	assertCovers(t, spans, 1000)
}

func TestPlanIgnoresUnusableSilence(t *testing.T) {
	// 5s after the start would make a chunk too short to be worth a request.
	spans := Plan(1000, 600, []float64{5})
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %v", len(spans), spans)
	}
	if spans[0].End != 500 {
		t.Errorf("first boundary = %v, want the even grid at 500", spans[0].End)
	}
}

func TestPlanIgnoresDistantSilence(t *testing.T) {
	// 200 seconds away from the grid point is outside the search window.
	spans := Plan(1000, 600, []float64{300})
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %v", len(spans), spans)
	}
	if spans[0].End != 500 {
		t.Errorf("first boundary = %v, want 500", spans[0].End)
	}
}

func TestPlanBalancesChunks(t *testing.T) {
	// 700s with a 600s target gives two 350s chunks rather than one 600s chunk
	// plus a 100s stub: the tail is more than 15% of a chunk.
	spans := Plan(700, 600, nil)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %v", len(spans), spans)
	}
	if spans[0].Duration() != 350 || spans[1].Duration() != 350 {
		t.Errorf("spans = %v, want two 350s halves", spans)
	}
	assertCovers(t, spans, 700)
}

func TestPlanClampsTinyTarget(t *testing.T) {
	spans := Plan(90, 1, nil)
	if len(spans) == 0 {
		t.Fatal("no spans")
	}
	for _, s := range spans {
		if s.Duration() < MinTarget-0.001 && s.End != 90 {
			t.Errorf("span %v is shorter than the minimum target", s)
		}
	}
	assertCovers(t, spans, 90)
}

func TestPlanWithUnsortedCuts(t *testing.T) {
	spans := Plan(1800, 600, []float64{1200, 590, 5, 1799})
	if len(spans) < 3 {
		t.Fatalf("got %d spans, want at least 3: %v", len(spans), spans)
	}
	if spans[0].End != 590 {
		t.Errorf("first boundary = %v, want 590", spans[0].End)
	}
	assertCovers(t, spans, 1800)
}

func TestSpanFormatting(t *testing.T) {
	s := Span{Start: 0, End: 3725}
	if got := s.String(); got != "0:00:00-1:02:05" {
		t.Errorf("String = %q", got)
	}
	if got := (Span{Start: 10.5, End: 20}).Duration(); math.Abs(got-9.5) > 1e-9 {
		t.Errorf("Duration = %v, want 9.5", got)
	}
}

// assertCovers checks that spans are contiguous, non-overlapping and reach the
// end of the recording: a gap would drop speech from the transcript.
func assertCovers(t *testing.T, spans []Span, duration float64) {
	t.Helper()
	if len(spans) == 0 {
		t.Fatal("no spans")
	}
	if spans[0].Start != 0 {
		t.Errorf("first span starts at %v, want 0", spans[0].Start)
	}
	for i := 1; i < len(spans); i++ {
		if spans[i].Start != spans[i-1].End {
			t.Errorf("gap or overlap between %v and %v", spans[i-1], spans[i])
		}
	}
	if last := spans[len(spans)-1].End; math.Abs(last-duration) > 1e-9 {
		t.Errorf("last span ends at %v, want %v", last, duration)
	}
	for _, s := range spans {
		if s.Duration() <= 0 {
			t.Errorf("span %v has no length", s)
		}
	}
}
