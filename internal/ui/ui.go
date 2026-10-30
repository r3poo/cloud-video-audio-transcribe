//go:build windows

// Package ui draws the desktop window and drives transcription jobs.
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/walk/declarative"

	"cloudtranscribe/internal/envfile"
	"cloudtranscribe/internal/ffmpeg"
	"cloudtranscribe/internal/openrouter"
	"cloudtranscribe/internal/transcribe"
)

const appName = "Cloud Transcribe"

// Keys the program reads from the environment or .env.
const (
	keyAPIKey    = "OPENROUTER_API_KEY"
	keyModel     = "OPENROUTER_MODEL"
	keyChunkSize = "TRANSCRIBE_CHUNK_SECONDS"
	keyOutputDir = "TRANSCRIBE_OUTPUT_DIR"
)

type app struct {
	mw      *walk.MainWindow
	env     *envfile.Values
	envPath string
	client  *openrouter.Client
	install *ffmpeg.Install
	models  []openrouter.Model

	busy     bool
	cancel   context.CancelFunc
	started  time.Time
	lastStep string
	lastDir  string
	fileDir  string

	lblFFmpeg   *walk.Label
	cmbModel    *walk.ComboBox
	lblModel    *walk.Label
	btnReload   *walk.PushButton
	btnPick     *walk.PushButton
	lineFile    *walk.LineEdit
	btnBrowse   *walk.PushButton
	lineOut     *walk.LineEdit
	btnOut      *walk.PushButton
	btnRun      *walk.PushButton
	btnCancel   *walk.PushButton
	btnOpen     *walk.PushButton
	progressBar *walk.ProgressBar
	lblProgress *walk.Label
	logBox      *walk.TextEdit
}

// Run creates the window, starts the environment checks and enters the Windows
// message loop.
func Run() {
	a := &app{}
	if err := a.build(); err != nil {
		walk.MsgBox(nil, appName, "Could not start the window:\r\n\r\n"+err.Error(), walk.MsgBoxIconError)
		os.Exit(1)
	}
	a.startup()
	a.mw.Run()
}

func (a *app) build() error {
	a.loadEnvironment()

	width, height := 780, 660
	return (declarative.MainWindow{
		AssignTo: &a.mw,
		Title:    appName,
		Size:     declarative.Size{Width: width, Height: height},
		MinSize:  declarative.Size{Width: 640, Height: 560},
		OnDropFiles: func(files []string) {
			a.acceptDroppedFiles(files)
		},
		Layout: declarative.VBox{Spacing: 8},
		Children: []declarative.Widget{
			declarative.Label{
				AssignTo: &a.lblFFmpeg,
				Text:     "Checking for ffmpeg…",
			},
			declarative.GroupBox{
				Title:  "Transcription model",
				Layout: declarative.VBox{Spacing: 4},
				Children: []declarative.Widget{
					declarative.Composite{
						Layout: declarative.HBox{Spacing: 8},
						Children: []declarative.Widget{
							declarative.ComboBox{
								AssignTo:              &a.cmbModel,
								Model:                 []string{"(loading models…)"},
								StretchFactor:         1,
								OnCurrentIndexChanged: a.describeModel,
							},
							declarative.PushButton{
								AssignTo:  &a.btnPick,
								Text:      "Change…",
								OnClicked: a.pickModelDialog,
							},
							declarative.PushButton{
								AssignTo:  &a.btnReload,
								Text:      "Reload list",
								OnClicked: a.reloadModels,
							},
						},
					},
					declarative.Label{
						AssignTo: &a.lblModel,
						Text:     " ",
					},
				},
			},
			declarative.GroupBox{
				Title:  "Input file (audio or video)",
				Layout: declarative.HBox{Spacing: 8},
				Children: []declarative.Widget{
					declarative.LineEdit{
						AssignTo:      &a.lineFile,
						ReadOnly:      true,
						StretchFactor: 1,
					},
					declarative.PushButton{
						AssignTo:  &a.btnBrowse,
						Text:      "Browse…",
						OnClicked: a.pickFile,
					},
				},
			},
			declarative.GroupBox{
				Title:  "Output folder (empty = next to the input file)",
				Layout: declarative.HBox{Spacing: 8},
				Children: []declarative.Widget{
					declarative.LineEdit{
						AssignTo:      &a.lineOut,
						StretchFactor: 1,
					},
					declarative.PushButton{
						AssignTo:  &a.btnOut,
						Text:      "Choose…",
						OnClicked: a.pickOutputDir,
					},
				},
			},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 8},
				Children: []declarative.Widget{
					declarative.PushButton{
						AssignTo:  &a.btnRun,
						Text:      "Transcribe",
						OnClicked: a.startRun,
					},
					declarative.PushButton{
						AssignTo:  &a.btnCancel,
						Text:      "Cancel",
						Enabled:   false,
						OnClicked: a.requestCancel,
					},
					declarative.HSpacer{},
					declarative.PushButton{
						AssignTo:  &a.btnOpen,
						Text:      "Open results folder",
						Enabled:   false,
						OnClicked: a.openResultsFolder,
					},
				},
			},
			declarative.Composite{
				Layout: declarative.HBox{Spacing: 8},
				Children: []declarative.Widget{
					declarative.ProgressBar{
						AssignTo:      &a.progressBar,
						MinValue:      0,
						MaxValue:      100,
						StretchFactor: 1,
					},
					declarative.Label{
						AssignTo: &a.lblProgress,
						Text:     "Idle",
						MinSize:  declarative.Size{Width: 240},
					},
				},
			},
			declarative.TextEdit{
				AssignTo:      &a.logBox,
				ReadOnly:      true,
				VScroll:       true,
				StretchFactor: 1,
				MinSize:       declarative.Size{Height: 150},
			},
		},
	}).Create()
}

// loadEnvironment reads .env files and builds the API client when a key exists.
func (a *app) loadEnvironment() {
	paths := envfile.CandidatePaths(executableDir(), configDir())
	values, err := envfile.Load(paths...)
	if err != nil {
		// A malformed .env is worth reporting, but the window still works.
		walk.MsgBox(nil, appName, err.Error(), walk.MsgBoxIconWarning)
	}
	if values == nil {
		values = &envfile.Values{}
	}
	a.env = values
	if len(values.Paths) > 0 {
		a.envPath = values.Paths[0]
	} else {
		a.envPath = filepath.Join(preferredEnvDir(), ".env")
	}

	if key := values.Get(keyAPIKey); key != "" {
		a.client = openrouter.New(key)
		a.client.OnRetry = func(attempt int, delay time.Duration, err error) {
			a.logf("Request failed (%v); retry %d in %s", err, attempt, delay.Round(time.Second))
		}
	}
}

// startup kicks off the two slow checks in the background so the window is
// interactive immediately.
func (a *app) startup() {
	a.logf("%s ready. ffmpeg, API key and models are being checked.", appName)
	if a.envPath != "" {
		a.logf("Settings file: %s", a.envPath)
	}
	go a.prepareFFmpeg()
	go a.prepareModels()
}

func (a *app) shutdown() {
	if a.cancel != nil {
		a.cancel()
	}
}

// --- ffmpeg ---------------------------------------------------------------

func (a *app) prepareFFmpeg() {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	exeDir := executableDir()
	if install, ok := ffmpeg.Resolve(ctx, exeDir); ok {
		a.ui(func() {
			a.install = install
			a.lblFFmpeg.SetText(install.Describe())
			a.appendLog("Using " + install.Describe())
			a.updateRunButton()
		})
		return
	}

	a.logf("No local ffmpeg found. Downloading a static Windows build (about 115 MB)…")
	var lastReport time.Time
	install, err := ffmpeg.Ensure(ctx, exeDir, func(p ffmpeg.Progress) {
		if p.Stage != "downloading" {
			a.logf("ffmpeg: %s (%s)", p.Stage, p.Source)
			return
		}
		if time.Since(lastReport) < 700*time.Millisecond {
			return
		}
		lastReport = time.Now()
		text := fmt.Sprintf("Downloading ffmpeg from %s: %.1f MB", p.Source, float64(p.Downloaded)/1e6)
		if p.Total > 0 {
			text = fmt.Sprintf("Downloading ffmpeg from %s: %.1f / %.1f MB", p.Source, float64(p.Downloaded)/1e6, float64(p.Total)/1e6)
		}
		a.ui(func() { a.lblFFmpeg.SetText(text) })
	})
	if err != nil {
		a.ui(func() {
			a.lblFFmpeg.SetText("ffmpeg unavailable — transcription is disabled")
			a.appendLog("ffmpeg error: " + err.Error())
			walk.MsgBox(a.mw, appName,
				"ffmpeg could not be found or downloaded:\r\n\r\n"+err.Error()+
					"\r\n\r\nInstall ffmpeg (https://ffmpeg.org/download.html) and restart, "+
					"or set FFMPEG_PATH to its location.", walk.MsgBoxIconError)
		})
		return
	}

	a.ui(func() {
		a.install = install
		a.lblFFmpeg.SetText(install.Describe())
		a.appendLog("Using " + install.Describe())
		a.updateRunButton()
	})
}

// --- models ---------------------------------------------------------------

// prepareModels fetches the transcription models and asks the user to pick one
// when no choice has been remembered.
func (a *app) prepareModels() {
	if a.client == nil {
		a.ui(func() {
			a.appendLog("No API key found. Add this line to " + a.envPath + ":")
			a.appendLog("    " + keyAPIKey + "=sk-or-v1-…")
			a.appendLog("Then restart the program. Get a key from https://openrouter.ai/keys")
			a.cmbModel.SetModel([]string{"(no API key)"})
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	models, err := a.client.ListTranscriptionModels(ctx)
	if err != nil {
		a.ui(func() {
			a.appendLog("Could not load models: " + err.Error())
			a.cmbModel.SetModel([]string{"(model list unavailable)"})
			walk.MsgBox(a.mw, appName, "Could not fetch the model list:\r\n\r\n"+err.Error(), walk.MsgBoxIconWarning)
		})
		return
	}

	a.ui(func() {
		a.models = models
		labels := modelLabels(models)
		_ = a.cmbModel.SetModel(labels)

		saved := a.env.Get(keyModel)
		index := -1
		if saved != "" {
			index = indexOfModel(models, saved)
		}
		remembered := index >= 0
		if !remembered {
			index = openrouter.DefaultModelIndex(models)
		}
		_ = a.cmbModel.SetCurrentIndex(index)
		a.appendLog(fmt.Sprintf("Loaded %d transcription models.", len(models)))
		a.updateRunButton()

		// First run: the requirement is to let the user pick, so show the list.
		if !remembered {
			a.pickModelDialog()
		}
	})
}

func (a *app) reloadModels() {
	if a.busy {
		return
	}
	if a.client == nil {
		walk.MsgBox(a.mw, appName,
			"No API key is configured. Add "+keyAPIKey+" to "+a.envPath+" and restart.",
			walk.MsgBoxIconWarning)
		return
	}
	_ = a.cmbModel.SetModel([]string{"(reloading…)"})
	go a.prepareModels()
}

// describeModel shows the identifier and price of the selected model.
func (a *app) describeModel() {
	index := a.cmbModel.CurrentIndex()
	if index < 0 || index >= len(a.models) {
		a.lblModel.SetText(" ")
		return
	}
	model := a.models[index]
	a.lblModel.SetText(model.ID + "  ·  " + model.PriceLabel())
}

// --- files ----------------------------------------------------------------

func (a *app) pickFile() {
	dlg := walk.FileDialog{
		Title:          "Choose an audio or video file",
		Filter:         mediaFilter(),
		InitialDirPath: a.fileDir,
	}
	accepted, err := dlg.ShowOpen(a.mw)
	if err != nil {
		a.logf("File dialog error: %v", err)
		return
	}
	if !accepted {
		return
	}
	a.setInput(dlg.FilePath)
}

func (a *app) pickOutputDir() {
	dlg := walk.FileDialog{
		Title:          "Choose where to write transcripts",
		InitialDirPath: a.lineOut.Text(),
	}
	accepted, err := dlg.ShowBrowseFolder(a.mw)
	if err != nil {
		a.logf("Folder dialog error: %v", err)
		return
	}
	if accepted {
		_ = a.lineOut.SetText(dlg.FilePath)
	}
}

func (a *app) acceptDroppedFiles(files []string) {
	if a.busy || len(files) == 0 {
		return
	}
	a.setInput(files[0])
	if len(files) > 1 {
		a.appendLog(fmt.Sprintf("Dropped %d files; using %s. Transcribe the others one at a time.", len(files), filepath.Base(files[0])))
	}
}

func (a *app) setInput(path string) {
	if path == "" {
		return
	}
	a.fileDir = filepath.Dir(path)
	_ = a.lineFile.SetText(path)
	a.appendLog("Selected: " + path)
	a.updateRunButton()
}

// --- running --------------------------------------------------------------

func (a *app) startRun() {
	if a.busy {
		return
	}
	input := strings.TrimSpace(a.lineFile.Text())
	if input == "" {
		walk.MsgBox(a.mw, appName, "Choose an audio or video file first (or drag one onto the window).", walk.MsgBoxIconInformation)
		return
	}
	if _, err := os.Stat(input); err != nil {
		walk.MsgBox(a.mw, appName, "The selected file cannot be read:\r\n\r\n"+err.Error(), walk.MsgBoxIconWarning)
		return
	}
	if a.install == nil {
		walk.MsgBox(a.mw, appName, "ffmpeg is not ready yet. Wait for the download to finish, or install ffmpeg and restart.", walk.MsgBoxIconWarning)
		return
	}
	index := a.cmbModel.CurrentIndex()
	if index < 0 || index >= len(a.models) {
		walk.MsgBox(a.mw, appName, "Pick a transcription model from the list first.", walk.MsgBoxIconWarning)
		return
	}

	opts := transcribe.Options{
		Model:     a.models[index].ID,
		OutputDir: strings.TrimSpace(a.lineOut.Text()),
		Language:  a.env.Get("TRANSCRIBE_LANGUAGE"),
	}
	if raw := a.env.Get(keyChunkSize); raw != "" {
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
			opts.ChunkSeconds = seconds
		}
	}
	if opts.OutputDir == "" {
		opts.OutputDir = a.env.Get(keyOutputDir)
	}

	// Remember the model and the output folder for the next run.
	a.remember(keyModel, opts.Model)
	if opts.OutputDir != "" {
		a.remember(keyOutputDir, opts.OutputDir)
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.started = time.Now()
	a.lastStep = ""
	a.setBusy(true)
	a.appendLog("Starting transcription with " + opts.Model)

	install := a.install
	client := a.client
	go func() {
		defer cancel()
		job := &transcribe.Job{
			FFmpeg: install,
			Client: client,
			Progress: func(p transcribe.Progress) {
				a.ui(func() { a.showProgress(p) })
			},
		}
		result, err := job.Run(ctx, transcribe.Input{Path: input}, opts)
		a.ui(func() { a.finishRun(result, err) })
	}()
}

func (a *app) requestCancel() {
	if a.cancel == nil {
		return
	}
	a.appendLog("Cancelling…")
	a.cancel()
	a.btnCancel.SetEnabled(false)
}

func (a *app) showProgress(p transcribe.Progress) {
	if p.Message != "" && p.Message != a.lastStep {
		a.appendLog(p.Message)
		a.lastStep = p.Message
	}
	percent := int(p.Fraction * 100)
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	a.progressBar.SetValue(percent)
	label := p.Message
	if p.Total > 1 {
		label = fmt.Sprintf("%d%%  (%d of %d parts)", percent, p.Done+1, p.Total)
	}
	a.lblProgress.SetText(label)
}

func (a *app) finishRun(result *transcribe.Result, err error) {
	a.setBusy(false)
	a.cancel = nil

	if err != nil {
		if errors.Is(err, context.Canceled) {
			a.appendLog("Cancelled. No files were written.")
			a.lblProgress.SetText("Cancelled")
			a.progressBar.SetValue(0)
			return
		}
		a.appendLog("Failed: " + err.Error())
		a.lblProgress.SetText("Failed")
		a.progressBar.SetValue(0)
		message := err.Error()
		var apiErr *openrouter.APIError
		if errors.As(err, &apiErr) {
			if hint := apiErr.HumanHint(); hint != "" {
				message = hint + "\r\n\r\n" + apiErr.Error()
			}
		}
		walk.MsgBox(a.mw, appName, message, walk.MsgBoxIconError)
		return
	}

	elapsed := time.Since(a.started).Round(time.Second)
	a.progressBar.SetValue(100)
	a.lblProgress.SetText(fmt.Sprintf("Done in %s — %d cues", elapsed, result.Segments))
	a.appendLog(fmt.Sprintf("Wrote %s", result.SRTPath))
	a.appendLog(fmt.Sprintf("Wrote %s", result.TextPath))
	a.appendLog(fmt.Sprintf("Transcribed %s in %d part(s), %d cues%s",
		humanDuration(result.Duration), result.Spans, result.Segments, costSuffix(result.CostUSD)))
	if len(result.Failed) > 0 {
		a.appendLog(fmt.Sprintf("Warning: %d part(s) failed and are missing from the output.", len(result.Failed)))
	}

	a.lastDir = filepath.Dir(result.SRTPath)
	a.btnOpen.SetEnabled(true)

	if len(result.Failed) > 0 {
		walk.MsgBox(a.mw, appName,
			fmt.Sprintf("The transcript was written, but %d part(s) of the recording could not be transcribed.\r\n\r\n%s",
				len(result.Failed), result.SRTPath), walk.MsgBoxIconWarning)
	}
}

func (a *app) openResultsFolder() {
	if a.lastDir == "" {
		return
	}
	if err := exec.Command("explorer.exe", a.lastDir).Start(); err != nil {
		a.logf("Could not open the folder: %v", err)
	}
}

// --- helpers --------------------------------------------------------------

func (a *app) ui(f func()) { a.mw.Synchronize(f) }

func (a *app) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	a.ui(func() { a.appendLog(line) })
}

func (a *app) appendLog(line string) {
	line = strings.TrimRight(line, "\r\n")
	if a.logBox.Text() != "" {
		line = "\r\n" + line
	}
	a.logBox.AppendText(line)
	a.logBox.ScrollToCaret()
}

func (a *app) setBusy(busy bool) {
	a.busy = busy
	a.btnBrowse.SetEnabled(!busy)
	a.btnOut.SetEnabled(!busy)
	a.btnReload.SetEnabled(!busy)
	a.btnPick.SetEnabled(!busy && len(a.models) > 0)
	a.btnCancel.SetEnabled(busy)
	a.cmbModel.SetEnabled(!busy)
	a.lineFile.SetEnabled(!busy)
	a.lineOut.SetEnabled(!busy)
	if busy {
		a.progressBar.SetValue(0)
		a.lblProgress.SetText("Starting…")
	}
	a.updateRunButton()
}

func (a *app) updateRunButton() {
	ready := !a.busy && a.install != nil && len(a.models) > 0 && a.client != nil
	a.btnRun.SetEnabled(ready)
}

// remember stores a setting so the next run starts where this one left off.
func (a *app) remember(key, value string) {
	if value == "" {
		return
	}
	a.env.Set(key, value)
	path := a.envPath
	if path == "" {
		path = filepath.Join(preferredEnvDir(), ".env")
	}
	if err := envfile.SetKey(path, key, value); err != nil {
		a.appendLog(fmt.Sprintf("Could not save %s to %s: %v", key, path, err))
		return
	}
	a.envPath = path
}

func modelLabels(models []openrouter.Model) []string {
	labels := make([]string, len(models))
	for i, m := range models {
		labels[i] = fmt.Sprintf("%s  ·  %s", m.DisplayName(), m.PriceLabel())
	}
	return labels
}

func indexOfModel(models []openrouter.Model, id string) int {
	for i, m := range models {
		if m.ID == id {
			return i
		}
	}
	return -1
}

func mediaFilter() string {
	patterns := make([]string, 0, len(transcribe.MediaExtensions))
	for _, ext := range transcribe.MediaExtensions {
		patterns = append(patterns, "*."+ext)
	}
	list := strings.Join(patterns, ";")
	return "Audio and video (" + list + ")|" + list + "|All files (*.*)|*.*"
}

func costSuffix(cost float64) string {
	if cost <= 0 {
		return ""
	}
	return fmt.Sprintf(", $%.4f", cost)
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	total := int(d.Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

func configDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "cloud-transcribe")
}

// preferredEnvDir is where a new .env is created: next to the program when that
// is writable (portable installs), otherwise the per-user config folder.
func preferredEnvDir() string {
	if dir := executableDir(); dir != "" && canWrite(dir) {
		return dir
	}
	if dir := configDir(); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		return dir
	}
	if dir, err := os.UserHomeDir(); err == nil {
		return dir
	}
	return "."
}

func canWrite(dir string) bool {
	probe, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	_ = os.Remove(name)
	return true
}
