package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloudtranscribe/internal/transcribe"
)

// mediaFiles creates real files so the queue's stat checks are exercised.
func mediaFiles(t *testing.T, names ...string) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, len(names))
	for i, name := range names {
		paths[i] = filepath.Join(dir, name)
		if err := os.WriteFile(paths[i], []byte("x"), 0o600); err != nil {
			t.Fatalf("creating %s: %v", paths[i], err)
		}
	}
	return paths
}

func TestQueueAddKeepsMediaAndReportsTheRest(t *testing.T) {
	media := mediaFiles(t, "talk.mp3", "film.MKV", "notes.txt")

	dir := t.TempDir()
	missing := filepath.Join(dir, "gone.mp4")

	q := &queue{}
	added, skipped := q.add([]string{media[0], media[1], media[2], dir, missing, media[0]})

	if added != 2 {
		t.Fatalf("added %d files, want 2 (mp3 and MKV, extension matching is case-insensitive)", added)
	}
	if q.len() != 2 {
		t.Fatalf("queue holds %d files, want 2", q.len())
	}

	reasons := map[string]string{}
	for _, s := range skipped {
		reasons[filepath.Base(s.path)] = s.reason
	}
	for name, want := range map[string]string{
		"notes.txt":        "not an audio or video file",
		filepath.Base(dir): "is a folder, not a file",
		"gone.mp4":         "cannot be read",
		"talk.mp3":         "already in the queue",
	} {
		if reasons[name] != want {
			t.Errorf("skip reason for %s = %q, want %q", name, reasons[name], want)
		}
	}
}

func TestQueueAddDeduplicatesRegardlessOfCase(t *testing.T) {
	paths := mediaFiles(t, "Talk.MP3")

	q := &queue{}
	added, skipped := q.add(paths)
	if added != 1 || len(skipped) != 0 {
		t.Fatalf("first add: added=%d skipped=%d, want 1 and 0", added, len(skipped))
	}

	// Windows paths are case-insensitive, so the same file must not queue twice.
	upper := strings.ToUpper(paths[0])
	if !strings.EqualFold(upper, paths[0]) {
		t.Fatalf("test setup: %q did not change case", paths[0])
	}
	added, skipped = q.add([]string{upper})
	if added != 0 || len(skipped) != 1 {
		t.Fatalf("re-adding an upper-cased path: added=%d skipped=%d, want 0 and 1", added, len(skipped))
	}
	if q.len() != 1 {
		t.Fatalf("queue holds %d files after the duplicate, want 1", q.len())
	}
}

func TestQueuePendingSkipsFinishedFiles(t *testing.T) {
	q := &queue{}
	q.add(mediaFiles(t, "a.mp3", "b.mp4", "c.wav"))

	q.setStatus(0, statusDone, "3 cues")
	q.setStatus(1, statusFailed, "no audio stream")

	if got := q.pending(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("pending() = %v, want [1 2]: a failed file must be retried, a finished one must not", got)
	}
}

func TestQueueCountsSeparateWaitingFailingAndDone(t *testing.T) {
	q := &queue{}
	q.add(mediaFiles(t, "a.mp3", "b.mp4", "c.wav"))
	q.setStatus(0, statusDone, "")
	q.setStatus(1, statusFailed, "")
	q.setStatus(2, statusRunning, "")

	done, failed, waiting := q.counts()
	if done != 1 || failed != 1 || waiting != 1 {
		t.Fatalf("counts = (%d, %d, %d), want (1, 1, 1)", done, failed, waiting)
	}
}

// A cancelled file must stay retryable, so it counts as waiting, not as failed.
func TestQueueCountsTreatCancelledFilesAsWaiting(t *testing.T) {
	q := &queue{}
	q.add(mediaFiles(t, "a.mp3", "b.mp4"))
	q.setStatus(0, statusDone, "")
	q.setStatus(1, statusCancelled, "cancelled")

	done, failed, waiting := q.counts()
	if done != 1 || failed != 0 || waiting != 1 {
		t.Fatalf("counts = (%d, %d, %d), want (1, 0, 1)", done, failed, waiting)
	}
	if got := q.pending(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("pending() = %v, want [1]", got)
	}
}

func TestFailureSummaryListsOnlyFailedFiles(t *testing.T) {
	q := &queue{}
	q.add(mediaFiles(t, "good.mp3", "broken.mp4"))
	q.setStatus(0, statusDone, "12 cues")
	q.setStatus(1, statusFailed, "no audio stream")

	summary := q.failureSummary()
	if !strings.Contains(summary, "broken.mp4") || !strings.Contains(summary, "no audio stream") {
		t.Fatalf("summary %q does not describe the failure", summary)
	}
	if strings.Contains(summary, "good.mp3") {
		t.Fatalf("summary %q mentions a successful file", summary)
	}
	if (&queue{}).failureSummary() != "" {
		t.Fatal("an empty queue reported failures")
	}
}

func TestFirstLineCollapsesMultiLineErrors(t *testing.T) {
	cases := map[string]string{
		"stream error\r\nsecond line": "stream error",
		"  padded  ":                  "padded",
		strings.Repeat("x", 300):      strings.Repeat("x", 119) + "…",
	}
	for in, want := range cases {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQueueRemoveAndClear(t *testing.T) {
	q := &queue{}
	q.add(mediaFiles(t, "a.mp3", "b.mp4", "c.wav"))

	if !q.removeAt(1) {
		t.Fatal("removeAt(1) refused a valid index")
	}
	if got := q.labels(); len(got) != 2 || !strings.Contains(got[0], "a.mp3") || !strings.Contains(got[1], "c.wav") {
		t.Fatalf("labels after removing b.mp4 = %v", got)
	}
	if q.removeAt(5) {
		t.Error("removeAt(5) accepted an out-of-range index")
	}

	q.clear()
	if q.len() != 0 || q.summary() != "No files queued" {
		t.Fatalf("after clear: len=%d summary=%q", q.len(), q.summary())
	}
}

func TestQueueLabelsShowStatusAndDetail(t *testing.T) {
	q := &queue{}
	q.add(mediaFiles(t, "interview.mp3"))
	q.setStatus(0, statusDone, "42 cues · interview.srt")

	label := q.labels()[0]
	for _, want := range []string{"done", "interview.mp3", "42 cues"} {
		if !strings.Contains(label, want) {
			t.Errorf("label %q does not mention %q", label, want)
		}
	}
}

// The bug this guards: a disabled Transcribe button that does not say why.
func TestEvaluateReadinessNamesTheMissingPiece(t *testing.T) {
	ready := readinessInput{hasKey: true, hasFFmpeg: true, modelCount: 3, modelIndex: 0, queueLen: 2, pendingLen: 2}

	if got := evaluateReadiness(ready); !got.ready || got.reason != "" {
		t.Fatalf("all requirements met: ready=%v reason=%q, want ready with no reason", got.ready, got.reason)
	}

	cases := []struct {
		name  string
		alter func(*readinessInput)
		want  string
	}{
		{"no key", func(in *readinessInput) { in.hasKey = false }, "API key"},
		{"no ffmpeg", func(in *readinessInput) { in.hasFFmpeg = false }, "ffmpeg"},
		{"no models", func(in *readinessInput) { in.modelCount = 0 }, "models"},
		{"no selection", func(in *readinessInput) { in.modelIndex = -1 }, "Choose a transcription model"},
		{"no files", func(in *readinessInput) { in.queueLen = 0; in.pendingLen = 0 }, "Add at least one"},
		{"nothing left to do", func(in *readinessInput) { in.pendingLen = 0 }, "All queued files are done"},
	}
	for _, tc := range cases {
		in := ready
		tc.alter(&in)
		got := evaluateReadiness(in)
		if got.ready {
			t.Errorf("%s: reported ready, want not ready", tc.name)
		}
		if !strings.Contains(got.reason, tc.want) {
			t.Errorf("%s: reason %q does not mention %q", tc.name, got.reason, tc.want)
		}
	}
}

// A run in progress must not report a missing requirement as the reason.
func TestEvaluateReadinessPrefersBusyOverMissingInputs(t *testing.T) {
	got := evaluateReadiness(readinessInput{busy: true})
	if got.ready {
		t.Fatal("busy state reported ready")
	}
	if got.reason != "Transcribing…" {
		t.Fatalf("reason = %q, want %q", got.reason, "Transcribing…")
	}
}

func TestIsMediaPathMatchesThePipelineExtensions(t *testing.T) {
	for _, path := range []string{"a.mp3", "a.MP4", "a.m4a", `C:\shows\b.mkv`} {
		if !isMediaPath(path) {
			t.Errorf("isMediaPath(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"a.txt", "a", "a.mp3.txt", "folder/"} {
		if isMediaPath(path) {
			t.Errorf("isMediaPath(%q) = true, want false", path)
		}
	}
	if len(transcribe.MediaExtensions) == 0 {
		t.Fatal("the pipeline advertises no media extensions")
	}
}
