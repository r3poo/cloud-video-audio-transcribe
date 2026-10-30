package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cloudtranscribe/internal/transcribe"
)

// This file deliberately has no build tag: the queue and the readiness rules
// are the parts worth testing, so they are kept free of the GUI toolkit and are
// exercised by `go test ./internal/ui` on any platform.

// itemStatus is where one queued file stands.
type itemStatus int

const (
	statusPending itemStatus = iota
	statusRunning
	statusDone
	statusFailed
	statusCancelled
)

func (s itemStatus) String() string {
	switch s {
	case statusRunning:
		return "running"
	case statusDone:
		return "done"
	case statusFailed:
		return "failed"
	case statusCancelled:
		return "cancelled"
	default:
		return "waiting"
	}
}

// queueItem is one file waiting to be transcribed.
type queueItem struct {
	path   string
	status itemStatus
	// note is the short detail shown after the file name: the error, or how
	// many cues were written and where.
	note string
}

func (it *queueItem) label() string {
	name := filepath.Base(it.path)
	if it.note == "" {
		return fmt.Sprintf("%-9s %s", it.status, name)
	}
	return fmt.Sprintf("%-9s %s  —  %s", it.status, name, it.note)
}

// queue is the list of files to transcribe, in the order they were added. It
// is only ever read and written from the UI thread.
type queue struct {
	items []*queueItem
}

func (q *queue) len() int { return len(q.items) }

func (q *queue) at(index int) *queueItem {
	if index < 0 || index >= len(q.items) {
		return nil
	}
	return q.items[index]
}

// skip reports one path that was not queued, and why.
type skip struct {
	path   string
	reason string
}

// add queues the paths that are transcribable media files and reports the rest
// instead of dropping them silently: a user who drags a folder or a duplicate
// should be told what happened.
func (q *queue) add(paths []string) (added int, skipped []skip) {
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			abs = path
		}
		if q.contains(abs) {
			skipped = append(skipped, skip{abs, "already in the queue"})
			continue
		}
		info, err := os.Stat(abs)
		switch {
		case err != nil:
			skipped = append(skipped, skip{abs, "cannot be read"})
			continue
		case info.IsDir():
			skipped = append(skipped, skip{abs, "is a folder, not a file"})
			continue
		case !isMediaPath(abs):
			skipped = append(skipped, skip{abs, "not an audio or video file"})
			continue
		}
		q.items = append(q.items, &queueItem{path: abs})
		added++
	}
	return added, skipped
}

func (q *queue) contains(path string) bool {
	for _, item := range q.items {
		if strings.EqualFold(item.path, path) {
			return true
		}
	}
	return false
}

func (q *queue) removeAt(index int) bool {
	if index < 0 || index >= len(q.items) {
		return false
	}
	q.items = append(q.items[:index], q.items[index+1:]...)
	return true
}

func (q *queue) clear() { q.items = nil }

// pending lists the files that still need transcribing. Finished files stay in
// the list so their results remain visible; only completed files are skipped.
func (q *queue) pending() []int {
	var out []int
	for i, item := range q.items {
		if item.status == statusDone {
			continue
		}
		out = append(out, i)
	}
	return out
}

// setStatus records the outcome of one file.
func (q *queue) setStatus(index int, status itemStatus, note string) {
	if item := q.at(index); item != nil {
		item.status = status
		item.note = note
	}
}

// labels renders one row per file for the list box.
func (q *queue) labels() []string {
	out := make([]string, len(q.items))
	for i, item := range q.items {
		out[i] = item.label()
	}
	return out
}

// counts summarises the queue. A cancelled file is not done, so it stays in the
// waiting count and is retried if the user runs the queue again.
func (q *queue) counts() (done, failed, waiting int) {
	for _, item := range q.items {
		switch item.status {
		case statusDone:
			done++
		case statusFailed:
			failed++
		default:
			waiting++
		}
	}
	return done, failed, waiting
}

func (q *queue) summary() string {
	done, failed, waiting := q.counts()
	switch len(q.items) {
	case 0:
		return "No files queued"
	case 1:
		switch {
		case waiting > 0:
			return "1 file queued"
		case failed > 0:
			return "1 file — failed"
		default:
			return "1 file — done"
		}
	}
	switch {
	case waiting > 0:
		return fmt.Sprintf("%d files — %d done, %d waiting", len(q.items), done, waiting)
	case failed > 0:
		return fmt.Sprintf("%d files — %d done, %d failed", len(q.items), done, failed)
	default:
		return fmt.Sprintf("%d files — all done", len(q.items))
	}
}

// failureSummary lists the files that did not transcribe, for the dialog shown
// when a batch ends. Long lists are truncated so the dialog stays readable.
func (q *queue) failureSummary() string {
	lines := make([]string, 0, len(q.items))
	for _, item := range q.items {
		if item.status != statusFailed {
			continue
		}
		line := filepath.Base(item.path)
		if item.note != "" {
			line += ": " + item.note
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	const maxLines = 10
	if len(lines) > maxLines {
		lines = append(lines[:maxLines:maxLines], fmt.Sprintf("… and %d more", len(lines)-maxLines))
	}
	return strings.Join(lines, "\r\n")
}

// firstLine trims a multi-line error down to one row's worth of text.
func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		text = text[:i]
	}
	const limit = 120
	if len(text) > limit {
		text = text[:limit-1] + "…"
	}
	return text
}

// isMediaPath reports whether the extension is one the program can transcribe.
func isMediaPath(path string) bool {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if ext == "" {
		return false
	}
	for _, known := range transcribe.MediaExtensions {
		if ext == known {
			return true
		}
	}
	return false
}

// readiness is the answer to "why is the Transcribe button grey?".
type readiness struct {
	ready bool
	// reason names the single unmet requirement. Empty when ready.
	reason string
}

// readinessInput is the state the decision depends on.
type readinessInput struct {
	hasKey     bool
	hasFFmpeg  bool
	modelCount int
	modelIndex int
	busy       bool
	queueLen   int
	pendingLen int
	// envPath is the .env file the program reads and writes, so the message can
	// name the exact file the key belongs in.
	envPath string
}

// evaluateReadiness decides whether a run can start. The checks are ordered so
// the reported reason is the first thing the user needs to fix; a button that is
// disabled without saying why is the bug this function exists to avoid.
func evaluateReadiness(in readinessInput) readiness {
	switch {
	case in.busy:
		return readiness{false, "Transcribing…"}
	case !in.hasKey:
		if in.envPath != "" {
			return readiness{false, "No API key. Put OPENROUTER_API_KEY=sk-or-v1-… in " + in.envPath + " and restart."}
		}
		return readiness{false, "No API key. Put OPENROUTER_API_KEY=sk-or-v1-… in the .env file next to the program, then restart."}
	case !in.hasFFmpeg:
		return readiness{false, "Waiting for ffmpeg: it is being located or downloaded."}
	case in.modelCount == 0:
		return readiness{false, "Waiting for the list of transcription models from OpenRouter."}
	case in.modelIndex < 0 || in.modelIndex >= in.modelCount:
		return readiness{false, "Choose a transcription model from the list."}
	case in.queueLen == 0:
		return readiness{false, "Add at least one audio or video file."}
	case in.pendingLen == 0:
		return readiness{false, "All queued files are done. Add more files, or clear the queue."}
	default:
		return readiness{true, ""}
	}
}
