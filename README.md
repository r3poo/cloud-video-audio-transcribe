# cloud-transcribe

A small Windows program that turns audio and video files into subtitles. It
drives ffmpeg locally to cut the media into API-sized pieces and sends each
piece to [OpenRouter](https://openrouter.ai) for transcription, then writes a
`.srt` subtitle file and a plain `.txt` transcript next to the source file.

- Drag a file onto the window (or Browse), pick a model, press **Transcribe**.
- ffmpeg is found automatically; if it is missing, the program downloads a
  static build once and keeps it in `%LOCALAPPDATA%\cloudtranscribe\ffmpeg`.
- Media never touches the disk: chunks are decoded by ffmpeg and piped straight
  into the HTTP request body. Only the ffmpeg zip is a temporary file, and it is
  deleted after unpacking.

## Requirements

- Windows 10 or 11, 64-bit. (The code also compiles for Linux for development,
  but the GUI only exists on Windows.)
- An OpenRouter API key: <https://openrouter.ai/settings/keys>

## Setup

1. Put `cloud-transcribe.exe` in a folder of its own.
2. Copy `.env.example` to `.env` and paste your key:

   ```
   OPENROUTER_API_KEY=sk-or-v1-...
   ```

3. Start the program. It checks for ffmpeg, then loads the list of models that
   accept audio input.

## Using it

1. **Pick the file** — drag it onto the window, or use *Browse…*. Supported
   containers: mp3, wav, m4a, aac, flac, ogg/opus, wma, mp4, mkv, mov, avi,
   webm, wmv, mpg, ts, 3gp and a few more. Anything ffmpeg can decode works.
2. **Pick a model** — *Change…* opens the list fetched from
   `/api/v1/models`, filtered to models that declare audio input. The list shows
   the price per audio token/hour where the model publishes it. Whisper-family
   models (`openai/whisper-large-v3`, `openai/whisper-1`, …) are the usual
   choice; free variants exist but are rate limited.
3. **Press Transcribe.** The progress bar covers probing, cutting audio,
   uploading and writing. *Cancel* stops after the chunk in flight.
4. **Open results folder** reveals `<name>.srt` and `<name>.txt`. If a
   transcript with that name already exists, the new one is written as
   `<name>-2.srt` instead of overwriting it.

Long recordings are split into ~10 minute spans, balanced so the last chunk is
not a stub, and each boundary snaps to a detected silence so words are not cut
in half. A span that fails (for example a rate limit) is reported in the log and
the remaining spans still transcribe.

## Configuration

Everything lives in `.env` next to the executable. The program writes the
chosen model and output folder back into that file, keeping your comments.

| Key | Meaning |
| --- | --- |
| `OPENROUTER_API_KEY` | required |
| `OPENROUTER_MODEL` | default model; also saved when you pick one |
| `TRANSCRIBE_LANGUAGE` | ISO-639-1 hint, empty = auto-detect |
| `TRANSCRIBE_CHUNK_SECONDS` | target chunk length, default 600 |
| `TRANSCRIBE_OUTPUT_DIR` | where results go, empty = next to the source |

`FFMPEG_PATH` (environment variable) forces a specific ffmpeg binary.

## Privacy and cost

Audio is uploaded to OpenRouter and forwarded to the model provider you pick, so
the recording leaves your machine. Each run logs the returned cost per chunk;
the log view shows the total when a run finishes. Nothing else is sent, and the
program keeps no copy of the audio.

OpenRouter caps a single request at 25 MB. At the default 64 kbit/s mono, ten
minutes is about 4.8 MB, so the default chunking stays clear of the limit even
for multi-hour recordings.

## Building

Go 1.24 or newer:

```powershell
go build -trimpath -ldflags "-H windowsgui -s -w" -o cloud-transcribe.exe .
```

`-H windowsgui` hides the console window. `.\build.ps1` runs the same command
and then checks that the result is usable.

### The application manifest

`app.manifest` must be linked into the exe, because walk (like any Common
Controls 6 program) needs it to activate the newer control library. Without it
the window never opens and the program reports `TTM_ADDTOOL failed`. Go links it
through the generated resource objects in the project root:

```
app_windows_amd64.syso
app_windows_arm64.syso
```

They are committed, so a plain `go build` is enough. If you edit `app.manifest`,
regenerate them:

```powershell
go install github.com/akavel/rsrc@latest
rsrc -manifest app.manifest -arch amd64 -o app_windows_amd64.syso
rsrc -manifest app.manifest -arch arm64 -o app_windows_arm64.syso
```

`go test .` checks that the manifest and the resource objects agree, and
`build.ps1` checks the finished exe, so a build that would fail at startup is
caught before it reaches you.

Tests: `go test ./...`. The ffmpeg integration tests skip themselves when no
ffmpeg is installed; to include them, either install ffmpeg or point
`FFMPEG_PATH` at a static build.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `TTM_ADDTOOL failed` on start | The exe was built without `app.manifest`. See *The application manifest* above. |
| Model list is empty | The key was rejected or the network is blocked; the log line says which. |
| `ffmpeg could not be obtained` | Both mirrors are unreachable. Install ffmpeg manually (`winget install Gyan.FFmpeg`, `scoop install ffmpeg`, or a zip in `C:\ffmpeg\bin`) and restart. |
| `no audio stream` | The file is video-only or the container is broken; check that it plays elsewhere. |
| Empty transcript | The chosen model does not really support audio, even if it advertises it. Pick a Whisper model. |
| `400`/`422` from the API | A chunk was rejected, usually a rate limit or size limit. The span is logged and skipped; lower `TRANSCRIBE_CHUNK_SECONDS` if it repeats. |

## How it works

```
main_windows.go  →  internal/ui        walk GUI, model picker, run button
                    internal/ffmpeg    find/download ffmpeg, probe, silence detect, extract
                    internal/chunk     split a duration into balanced spans
                    internal/openrouter  model list + audio transcriptions
                    internal/subtitle  SRT formatting
                    internal/transcribe  the pipeline that ties it together
                    internal/envfile   tiny .env reader/writer
```

Design notes:

- The UI is [lxn/walk](https://github.com/lxn/walk), a thin Win32 wrapper with a
  declarative layout. It pulls in no runtime, ships as one exe, and needs no
  installer.
- ffmpeg is only used through its command line, so a user-installed build and a
  downloaded static build behave identically.
- Each chunk is a 64 kbit/s mono MP3 held in memory: small enough to fit in a
  JSON body, and every supported model accepts it.
- Requests use OpenRouter's documented `input_audio` JSON shape with
  `verbose_json` segments, which is what the audio-transcription endpoint
  accepts for base64 audio. Fallbacks cover providers that answer with plain
  JSON or wrap the payload in `{"data": …}`.
