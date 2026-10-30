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
	keySpeed     = "TRANSCRIBE_SPEED"
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
	queue    queue

	// ffmpegNote and modelsNote explain a failed check. They are only shown
	// when that check is what blocks the run.
	ffmpegNote string
	modelsNote string

	lblFFmpeg *walk.Label
	cmbModel  *walk.ComboBox
	lblModel  *walk.Label
	// cmbSpeed picks the playback factor that decides how many billed seconds
	// a recording costs.
	cmbSpeed    *walk.ComboBox
	lblSpeed    *walk.Label
	btnReload   *walk.PushButton
	lstQueue    *walk.ListBox
	lblQueue    *walk.Label
	btnAdd      *walk.PushButton
	btnRemove   *walk.PushButton
	btnClear    *walk.PushButton
	lineOut     *walk.LineEdit
	btnOut      *walk.PushButton
	btnRun      *walk.PushButton
	btnCancel   *walk.PushButton
	btnOpen     *walk.PushButton
	lblReady    *walk.Label
	progressBar *walk.ProgressBar
	lblProgress *walk.Label
	logBox      *walk.TextEdit
}

// Run creates the window, starts the environment checks and enters the Windows
// message loop.
func Run() {
	a := &app{}
	if err := a.build(); err != nil {
		walk.MsgBox(nil, appName, startupFailureMessage(err), walk.MsgBoxIconError)
		os.Exit(1)
	}
	a.startup()
	a.mw.Run()
}

// startupFailureMessage turns a window-creation error into something the user
// can act on. The one worth spelling out is TTM_ADDTOOL: walk registers every
// widget with a tooltip control, and that fails when the executable was linked
// without the Common Controls 6 manifest, so no window can be built at all.
func startupFailureMessage(err error) string {
	msg := "Could not start the window:\r\n\r\n" + err.Error()
	if strings.Contains(err.Error(), "TTM_ADDTOOL") || strings.Contains(err.Error(), "ADDTOOL") {
		msg += "\r\n\r\nThis executable was built without its application manifest, so Windows " +
			"does not activate Common Controls 6.0.\r\n\r\n" +
			"Rebuild it from the project root so app_windows_amd64.syso is linked in " +
			"(see README, Building), or drop app.manifest next to the exe as " +
			"cloud-transcribe.exe.manifest."
	}
	return msg
}

func (a *app) build() error {
	a.loadEnvironment()

	width, height := 800, 760
	return (declarative.MainWindow{
		AssignTo: &a.mw,
		Title:    appName,
		Size:     declarative.Size{Width: width, Height: height},
		MinSize:  declarative.Size{Width: 660, Height: 620},
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
				Title:  "Cost: send the audio faster",
				Layout: declarative.VBox{Spacing: 4},
				Children: []declarative.Widget{
					declarative.Composite{
						Layout: declarative.HBox{Spacing: 8},
						Children: []declarative.Widget{
							declarative.Label{
								Text: "Playback speed",
							},
							declarative.ComboBox{
								AssignTo:              &a.cmbSpeed,
								Model:                 speedLabels(),
								CurrentIndex:          0,
								OnCurrentIndexChanged: a.describeSpeed,
							},
							declarative.HSpacer{},
						},
					},
					declarative.Label{
						AssignTo: &a.lblSpeed,
						Text:     " ",
					},
				},
			},
			declarative.GroupBox{
				Title:  "Files to transcribe (or drag audio and video onto the window)",
				Layout: declarative.VBox{Spacing: 6},
				Children: []declarative.Widget{
					declarative.ListBox{
						AssignTo:              &a.lstQueue,
						StretchFactor:         1,
						MinSize:               declarative.Size{Height: 120},
						OnCurrentIndexChanged: a.queueSelectionChanged,
					},
					declarative.Composite{
						Layout: declarative.HBox{Spacing: 8},
						Children: []declarative.Widget{
							declarative.PushButton{
								AssignTo:  &a.btnAdd,
								Text:      "Add files…",
								OnClicked: a.addFiles,
							},
							declarative.PushButton{
								AssignTo:  &a.btnRemove,
								Text:      "Remove",
								Enabled:   false,
								OnClicked: a.removeSelected,
							},
							declarative.PushButton{
								AssignTo:  &a.btnClear,
								Text:      "Clear",
								Enabled:   false,
								OnClicked: a.clearQueue,
							},
							declarative.HSpacer{},
							declarative.Label{
								AssignTo: &a.lblQueue,
								Text:     "No files queued",
							},
						},
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
			declarative.Label{
				AssignTo: &a.lblReady,
				Text:     "Checking ffmpeg, API key and models…",
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
	a.refreshQueue()
	a.applySavedSpeed()
	a.refreshReadiness()
	a.describeSpeed()
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
			a.ffmpegNote = ""
			a.lblFFmpeg.SetText(install.Describe())
			a.appendLog("Using " + install.Describe())
			a.refreshReadiness()
		})
		return
	}

	a.logf("No local ffmpeg found. Downloading a static Windows build…")
	if dir, err := ffmpeg.CacheDir(); err == nil {
		a.logf("ffmpeg will be installed in %s (ffmpeg-download.zip appears there while it downloads)", dir)
	}
	var lastReport, lastLog time.Time
	var lastSource string
	install, err := ffmpeg.Ensure(ctx, exeDir, func(p ffmpeg.Progress) {
		switch p.Stage {
		case "requesting":
			if p.Source != lastSource {
				lastSource = p.Source
				a.logf("Contacting %s…", p.Source)
			}
			a.ui(func() { a.lblFFmpeg.SetText("Asking " + p.Source + " for ffmpeg…") })
			return
		case "extracting", "verifying":
			a.logf("ffmpeg: %s (%s)", p.Stage, p.Source)
			a.ui(func() { a.lblFFmpeg.SetText("Installing ffmpeg: " + p.Stage + "…") })
			return
		}

		text := fmt.Sprintf("Downloading ffmpeg from %s: %s", p.Source, ffmpeg.HumanBytes(p.Downloaded))
		if p.Total > 0 {
			text += " / " + ffmpeg.HumanBytes(p.Total)
		}
		text += " at " + ffmpeg.HumanRate(p.Speed)
		if p.Total > 0 && p.Speed > 0 {
			if left := time.Duration(float64(p.Total-p.Downloaded)/float64(p.Speed)) * time.Second; left > 0 {
				text += fmt.Sprintf(", about %s left", left.Round(time.Second))
			}
		}

		if time.Since(lastReport) >= 700*time.Millisecond {
			lastReport = time.Now()
			a.ui(func() { a.lblFFmpeg.SetText(text) })
		}
		// One log line every few seconds is enough to prove it is moving.
		if time.Since(lastLog) >= 10*time.Second {
			lastLog = time.Now()
			a.logf("%s", text)
		}
	})
	if err != nil {
		a.ui(func() {
			a.lblFFmpeg.SetText("ffmpeg unavailable — transcription is disabled")
			a.appendLog("ffmpeg error: " + err.Error())
			a.ffmpegNote = "ffmpeg could not be found or downloaded. Install it (winget install Gyan.FFmpeg), " +
				"or put a static build in ffmpeg\\bin next to the program, or set FFMPEG_PATH."
			a.refreshReadiness()
			walk.MsgBox(a.mw, appName,
				"ffmpeg could not be found or downloaded:\r\n\r\n"+err.Error()+
					"\r\n\r\nInstall ffmpeg (https://ffmpeg.org/download.html) and restart, "+
					"or set FFMPEG_PATH to its location.", walk.MsgBoxIconError)
		})
		return
	}

	a.ui(func() {
		a.install = install
		a.ffmpegNote = ""
		a.lblFFmpeg.SetText(install.Describe())
		a.appendLog("Using " + install.Describe())
		a.refreshReadiness()
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
			a.refreshReadiness()
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
			a.modelsNote = "Could not load the model list from OpenRouter: " + firstLine(err.Error())
			a.refreshReadiness()
			walk.MsgBox(a.mw, appName, "Could not fetch the model list:\r\n\r\n"+err.Error(), walk.MsgBoxIconWarning)
		})
		return
	}

	a.ui(func() {
		a.models = models
		a.modelsNote = ""
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
		a.refreshReadiness()

		// The dropdown holds the whole list, so a first run needs no dialog;
		// just point at it when the choice is still the default.
		if !remembered {
			a.appendLog("Pick the model in the dropdown. 2× playback speed halves the bill.")
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
		a.refreshReadiness()
		return
	}
	model := a.models[index]
	a.lblModel.SetText(model.ID + "  ·  " + model.PriceLabel())
	a.refreshReadiness()
}

// --- files ----------------------------------------------------------------

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

// addFiles queues every audio and video file the user selects.
func (a *app) addFiles() {
	if a.busy {
		return
	}
	dlg := walk.FileDialog{
		Title:          "Choose audio or video files",
		Filter:         mediaFilter(),
		InitialDirPath: a.fileDir,
	}
	accepted, err := dlg.ShowOpenMultiple(a.mw)
	if err != nil {
		a.logf("File dialog error: %v", err)
		return
	}
	if !accepted || len(dlg.FilePaths) == 0 {
		return
	}
	a.fileDir = filepath.Dir(dlg.FilePaths[len(dlg.FilePaths)-1])
	a.enqueue(dlg.FilePaths)
}

// acceptDroppedFiles queues everything dragged onto the window.
func (a *app) acceptDroppedFiles(files []string) {
	if a.busy || len(files) == 0 {
		return
	}
	a.enqueue(files)
}

// enqueue adds paths to the queue, logging what was rejected and why rather
// than dropping it silently.
func (a *app) enqueue(paths []string) {
	added, skipped := a.queue.add(paths)
	for _, s := range skipped {
		a.appendLog(fmt.Sprintf("Skipped %s: %s", filepath.Base(s.path), s.reason))
	}
	if added > 0 {
		a.appendLog(fmt.Sprintf("Queued %d file(s); %s.", added, a.queue.summary()))
	}
	a.refreshQueue()
	a.refreshReadiness()
}

func (a *app) removeSelected() {
	if a.busy {
		return
	}
	index := a.lstQueue.CurrentIndex()
	item := a.queue.at(index)
	if item == nil {
		return
	}
	if a.queue.removeAt(index) {
		a.appendLog("Removed " + filepath.Base(item.path))
	}
	a.refreshQueue()
	a.refreshReadiness()
}

func (a *app) clearQueue() {
	if a.busy || a.queue.len() == 0 {
		return
	}
	a.queue.clear()
	a.appendLog("Cleared the queue.")
	a.refreshQueue()
	a.refreshReadiness()
}

// --- running --------------------------------------------------------------

// target is one queued file, snapshotted on the UI thread before the worker
// goroutine starts, so the goroutine never touches the queue itself.
type target struct {
	index int
	path  string
}

func (a *app) startRun() {
	if a.busy {
		return
	}
	if state := a.readiness(); !state.ready {
		walk.MsgBox(a.mw, appName, state.reason, walk.MsgBoxIconInformation)
		return
	}

	var targets []target
	for _, index := range a.queue.pending() {
		if item := a.queue.at(index); item != nil {
			targets = append(targets, target{index: index, path: item.path})
		}
	}
	if len(targets) == 0 {
		walk.MsgBox(a.mw, appName,
			"Every queued file has already been transcribed.\r\n\r\nAdd more files, or clear the queue to start over.",
			walk.MsgBoxIconInformation)
		return
	}

	opts := a.options()

	// Remember the model, the factor and the output folder for the next run.
	a.remember(keyModel, opts.Model)
	a.remember(keySpeed, strconv.FormatFloat(opts.Speed, 'g', -1, 64))
	if opts.OutputDir != "" {
		a.remember(keyOutputDir, opts.OutputDir)
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.started = time.Now()
	a.lastStep = ""
	a.setBusy(true)
	a.appendLog(fmt.Sprintf("Starting %d file(s) with %s", len(targets), opts.Model))

	install := a.install
	client := a.client
	go func() {
		defer cancel()
		a.runQueue(ctx, targets, opts, install, client)
	}()
}

// options collects the settings for a run from the window and the .env file.
func (a *app) options() transcribe.Options {
	opts := transcribe.Options{
		OutputDir: strings.TrimSpace(a.lineOut.Text()),
		Language:  a.env.Get("TRANSCRIBE_LANGUAGE"),
	}
	if model := a.selectedModel(); model != nil {
		opts.Model = model.ID
	}
	if raw := a.env.Get(keyChunkSize); raw != "" {
		if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
			opts.ChunkSeconds = seconds
		}
	}
	opts.Speed = a.speedFactor()
	if opts.OutputDir == "" {
		opts.OutputDir = a.env.Get(keyOutputDir)
	}
	return opts
}

func (a *app) selectedModel() *openrouter.Model {
	index := a.cmbModel.CurrentIndex()
	if index < 0 || index >= len(a.models) {
		return nil
	}
	return &a.models[index]
}

// runQueue transcribes the files one after another. A failure is recorded and
// the queue continues, so one unreadable file does not cost the rest of the
// batch; cancelling stops the run instead of moving to the next file.
func (a *app) runQueue(ctx context.Context, targets []target, opts transcribe.Options, install *ffmpeg.Install, client *openrouter.Client) {
	total := len(targets)

	defer func() {
		a.ui(a.finishQueue)
	}()

	for position, item := range targets {
		if ctx.Err() != nil {
			return
		}
		index, path := item.index, item.path

		a.ui(func() { a.markItem(index, statusRunning, "") })

		job := &transcribe.Job{
			FFmpeg: install,
			Client: client,
			Progress: func(p transcribe.Progress) {
				overall := (float64(position) + clampFraction(p.Fraction)) / float64(total)
				a.ui(func() { a.showProgress(p, position+1, total, overall) })
			},
		}

		result, err := job.Run(ctx, transcribe.Input{Path: path}, opts)
		switch {
		case err == nil:
			a.ui(func() { a.markDone(index, result) })
		case errors.Is(err, context.Canceled):
			a.ui(func() {
				a.markItem(index, statusCancelled, "cancelled")
				a.appendLog("Cancelled " + filepath.Base(path))
			})
			return
		default:
			errText := err.Error()
			a.ui(func() {
				a.markItem(index, statusFailed, firstLine(errText))
				a.appendLog(fmt.Sprintf("Failed %s: %s", filepath.Base(path), errText))
				var apiErr *openrouter.APIError
				if errors.As(err, &apiErr) {
					if hint := apiErr.HumanHint(); hint != "" {
						a.appendLog(hint)
					}
				}
			})
		}
	}
}

func (a *app) markItem(index int, status itemStatus, note string) {
	a.queue.setStatus(index, status, note)
	a.refreshQueue()
}

// markDone records one successful file and makes its output reachable.
func (a *app) markDone(index int, result *transcribe.Result) {
	a.markItem(index, statusDone, fmt.Sprintf("%d cues · %s", result.Segments, filepath.Base(result.SRTPath)))
	a.lastDir = filepath.Dir(result.SRTPath)
	a.btnOpen.SetEnabled(true)
	a.appendLog("Wrote " + result.SRTPath)
	a.appendLog("Wrote " + result.TextPath)
	a.appendLog(fmt.Sprintf("Transcribed %s in %d part(s), %d cues%s",
		humanDuration(result.Duration), result.Spans, result.Segments, costSuffix(result.CostUSD)))
	if len(result.Failed) > 0 {
		a.appendLog(fmt.Sprintf("Warning: %d part(s) of %s failed and are missing from the output.",
			len(result.Failed), filepath.Base(result.SRTPath)))
	}
}

func clampFraction(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func (a *app) requestCancel() {
	if a.cancel == nil {
		return
	}
	a.appendLog("Cancelling…")
	a.cancel()
	a.btnCancel.SetEnabled(false)
}

func (a *app) showProgress(p transcribe.Progress, fileNo, files int, overall float64) {
	if p.Message != "" && p.Message != a.lastStep {
		a.appendLog(p.Message)
		a.lastStep = p.Message
	}
	percent := int(clampFraction(overall) * 100)
	a.progressBar.SetValue(percent)
	a.lblProgress.SetText(fmt.Sprintf("File %d of %d — %d%% — %s", fileNo, files, percent, p.Message))
}

// finishQueue reports how the batch went and re-enables the window.
func (a *app) finishQueue() {
	a.setBusy(false)
	a.cancel = nil

	done, failedCount, waiting := a.queue.counts()
	elapsed := time.Since(a.started).Round(time.Second)

	switch {
	case waiting > 0:
		a.lblProgress.SetText(fmt.Sprintf("Stopped — %d of %d file(s) done", done, done+failedCount+waiting))
		a.appendLog(fmt.Sprintf("Stopped after %s: %d done, %d failed, %d not attempted.",
			elapsed, done, failedCount, waiting))
	case failedCount > 0:
		a.progressBar.SetValue(100)
		a.lblProgress.SetText(fmt.Sprintf("Finished in %s — %d done, %d failed", elapsed, done, failedCount))
		a.appendLog(fmt.Sprintf("Finished in %s: %d file(s) done, %d failed.", elapsed, done, failedCount))
	default:
		a.progressBar.SetValue(100)
		a.lblProgress.SetText(fmt.Sprintf("Done in %s — %d file(s)", elapsed, done))
		a.appendLog(fmt.Sprintf("Finished %d file(s) in %s.", done, elapsed))
	}

	if failedCount > 0 {
		walk.MsgBox(a.mw, appName,
			fmt.Sprintf("%d of %d file(s) could not be transcribed:\r\n\r\n%s",
				failedCount, done+failedCount+waiting, a.queue.failureSummary()),
			walk.MsgBoxIconWarning)
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
	a.btnAdd.SetEnabled(!busy)
	a.btnOut.SetEnabled(!busy)
	a.btnReload.SetEnabled(!busy && a.client != nil)
	a.btnCancel.SetEnabled(busy)
	a.cmbModel.SetEnabled(!busy)
	a.cmbSpeed.SetEnabled(!busy)
	a.lineOut.SetEnabled(!busy)
	a.lstQueue.SetEnabled(!busy)
	if busy {
		a.progressBar.SetValue(0)
		a.lblProgress.SetText("Starting…")
	}
	a.refreshQueue()
	a.refreshReadiness()
}

// refreshQueue redraws the file list and the buttons that act on it.
func (a *app) refreshQueue() {
	selected := a.lstQueue.CurrentIndex()
	_ = a.lstQueue.SetModel(a.queue.labels())
	if n := a.queue.len(); selected >= n {
		selected = n - 1
	}
	if selected >= 0 {
		_ = a.lstQueue.SetCurrentIndex(selected)
	}
	a.lblQueue.SetText(a.queue.summary())
	a.queueSelectionChanged()
	a.btnClear.SetEnabled(!a.busy && a.queue.len() > 0)
}

// queueSelectionChanged only reacts to the selected row. It must not replace the
// model, because SetModel resets the selection and would re-enter this handler.
func (a *app) queueSelectionChanged() {
	a.btnRemove.SetEnabled(!a.busy && a.lstQueue.CurrentIndex() >= 0)
	a.refreshReadiness()
}

// readiness reports whether a run can start and what is missing if it cannot.
func (a *app) readiness() readiness {
	return evaluateReadiness(readinessInput{
		hasKey:     a.client != nil,
		hasFFmpeg:  a.install != nil,
		modelCount: len(a.models),
		modelIndex: a.cmbModel.CurrentIndex(),
		busy:       a.busy,
		queueLen:   a.queue.len(),
		pendingLen: len(a.queue.pending()),
		envPath:    a.envPath,
	})
}

// refreshReadiness keeps the Transcribe button and the line beneath it in sync.
// The line always names what is missing, so a disabled button explains itself.
func (a *app) refreshReadiness() {
	state := a.readiness()
	a.btnRun.SetEnabled(state.ready)

	if a.busy {
		a.lblReady.SetText("Transcribing…")
		return
	}
	if state.ready {
		name := "the selected model"
		if model := a.selectedModel(); model != nil {
			name = model.ID
		}
		a.lblReady.SetText(fmt.Sprintf("Ready: %d file(s) will be transcribed with %s.", a.queue.len(), name))
		return
	}

	// A check that failed says so, instead of reporting that it is still running.
	reason := state.reason
	switch {
	case a.install == nil && a.ffmpegNote != "":
		reason = a.ffmpegNote
	case len(a.models) == 0 && a.client != nil && a.modelsNote != "":
		reason = a.modelsNote
	}
	a.lblReady.SetText(reason)
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

// --- speed ---------------------------------------------------------------

// speedChoices are the playback factors offered in the window. Speeding the
// audio up shortens what the provider bills for; the high values are there for
// long lectures where a rough transcript beats paying twice.
var speedChoices = []struct {
	label  string
	factor float64
}{
	{"1× (original speed)", 1},
	{"1.25×", 1.25},
	{"1.5×", 1.5},
	{"2× (half the cost)", 2},
	{"2.5×", 2.5},
	{"3×", 3},
}

func speedLabels() []string {
	labels := make([]string, len(speedChoices))
	for i, c := range speedChoices {
		labels[i] = c.label
	}
	return labels
}

// speedFactor is the chosen factor. Out-of-range indexes mean normal speed.
func (a *app) speedFactor() float64 {
	index := a.cmbSpeed.CurrentIndex()
	if index < 0 || index >= len(speedChoices) {
		return 1
	}
	return speedChoices[index].factor
}

// applySavedSpeed selects the factor last used, so the choice survives a
// restart the way the model and the output folder do.
func (a *app) applySavedSpeed() {
	raw := a.env.Get(keySpeed)
	factor, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return
	}
	for i, choice := range speedChoices {
		if choice.factor == factor {
			_ = a.cmbSpeed.SetCurrentIndex(i)
			return
		}
	}
}

// describeSpeed spells out what the factor costs in money and accuracy, so the
// trade-off is visible before a run rather than in the bill.
func (a *app) describeSpeed() {
	factor := a.speedFactor()
	if factor <= 1 {
		a.lblSpeed.SetText("Original speed: one billed second per recorded second.")
		return
	}
	percent := int(100/factor + 0.5)
	a.lblSpeed.SetText(fmt.Sprintf(
		"Sent at %s×: about %d%% of the usual cost. Very fast speech, overlapping speakers or music may lose words.",
		strconv.FormatFloat(factor, 'g', -1, 64), percent))
}
