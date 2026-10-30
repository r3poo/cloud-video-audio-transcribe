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

1. **Add files** — drag them onto the window (as many as you like) or use
   *Add files…*, which accepts a multi-selection. Files are queued in the order
   you add them; duplicates, folders and non-media files are reported in the log
   rather than silently dropped. Supported containers: mp3, wav, m4a, aac, flac,
   ogg/opus, wma, mp4, mkv, mov, avi, webm, wmv, mpg, ts, 3gp and a few more.
   Anything ffmpeg can decode works.
2. **Pick a model** — the dropdown holds every model fetched from
   `/api/v1/models` that declares audio input, with the price per audio
   token/hour where the model publishes it. Whisper-family models
   (`openai/whisper-large-v3`, `openai/whisper-1`, …) are the usual choice; free
   variants exist but are rate limited. The choice is written to `.env` so it
   survives a restart.
3. **Optionally speed the audio up.** *Cost: send the audio faster* offers
   1.25×, 1.5×, 2× (half the price), 2.5× and 3×. The factor is applied by
   ffmpeg before upload with `atempo`, which compresses time without changing
   pitch, and subtitle timestamps are stretched back to the original timeline,
   so results still line up with the recording.
4. **Press Transcribe.** The queue is processed one file at a time. The progress
   bar covers the whole batch, and the line above it names the file being worked
   on. A file that fails is marked in the list and the batch continues; *Cancel*
   stops the batch instead of moving to the next file. Pressing Transcribe again
   retries whatever did not finish.
5. **Open results folder** reveals `<name>.srt` and `<name>.txt`. If a
   transcript with that name already exists, the new one is written as
   `<name>-2.srt` instead of overwriting it.

The line beneath the buttons always states why *Transcribe* is unavailable —
no API key, ffmpeg still downloading, model list not loaded, nothing queued —
so a grey button never needs guessing at.

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
| `TRANSCRIBE_SPEED` | playback factor sent to the model, default `1`; also saved when you change it |
| `TRANSCRIBE_AUDIO_FORMAT` | container to try first (`wav`, `mp3`, `flac`, `pcm16`); written after a successful run |
| `TRANSCRIBE_LANGUAGE` | ISO-639-1 hint, empty = auto-detect |
| `TRANSCRIBE_CHUNK_SECONDS` | target chunk length, default 600 |
| `TRANSCRIBE_OUTPUT_DIR` | where results go, empty = next to the source |

`FFMPEG_PATH` (environment variable) forces a specific ffmpeg binary.

## Privacy and cost

Audio is uploaded to OpenRouter and forwarded to the model provider you pick, so
the recording leaves your machine. Each run logs the returned cost per chunk;
the log view shows the total when a run finishes. Nothing else is sent, and the
program keeps no copy of the audio.

Providers bill by audio second, so the playback factor is a direct discount:
2× costs half, and the only price is accuracy on very fast or overlapping
speech. The factor is saved with the other settings.

OpenRouter caps a single request at 25 MB, and base64 adds a third on top. Ten
minutes of 16 kHz mono WAV is about 19 MB, so a span that would exceed 17 MB in
memory is split in half and sent as two requests rather than failing.

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
| *Transcribe* is grey | The line under the buttons says which requirement is missing. |
| ffmpeg download crawls or seems stuck | It fell back to www.gyan.dev after both GitHub mirrors failed; the status line shows the source and rate, and the source is dropped after 30 s if it stays under 120 KB/s. Check *Contacting…* lines in the log. |
| `TTM_ADDTOOL failed` on start | The exe was built without `app.manifest`. See *The application manifest* above. |
| Model list is empty | The key was rejected or the network is blocked; the log line says which. |
| `ffmpeg could not be obtained` | Both mirrors are unreachable. Install ffmpeg manually (`winget install Gyan.FFmpeg`, `scoop install ffmpeg`, or a zip in `C:\ffmpeg\bin`) and restart. |
| `no audio stream` | The file is video-only or the container is broken; check that it plays elsewhere. |
| Empty transcript | The chosen model does not really support audio, even if it advertises it. Pick a Whisper model. |
| `Provider could not process the audio input` or `unsupported or malformed audio` | Two different problems share this one message: a container the route cannot decode, and a *request option* it cannot honour (typically timestamped output). The program tries the plain request shape on the same bytes first — no re-encode — and only then walks the container list. If all four containers fail in both shapes, the model does not really support audio — switch to another one. |
| `400`/`422` from the API | A chunk was rejected, usually a rate limit or size limit. The span is logged and skipped; lower `TRANSCRIBE_CHUNK_SECONDS` if it repeats. |
| `413` or `payload too large` | The chunk outgrew the gateway's inline cap. It is halved and re-sent automatically; the guard that measures chunks usually prevents this, so it means the estimate was wrong for that file. |
| A model refuses everything, on every file | `nvidia/nemotron-3.5-transcribe` currently answers `400` for every container, with and without timestamps, on requests that `nvidia/parakeet-tdt-0.6b-v3` and `openai/whisper-large-v3-turbo` accept unchanged. It is a fault on the route, not in the file: switch models. The program stops the file after the first span's ladder instead of paying for the same rejections on every chunk. |
| `Access is denied` when writing the `.srt` | The media folder refuses that name. The program now steps to `name (2).srt`, and if no name in the folder works it writes to `%LOCALAPPDATA%\cloudtranscribe\transcripts` and says so in the log, so a paid-for transcript is never lost. |

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
- Each chunk is exported to memory and sent as WAV first, then MP3, FLAC and raw
  16-bit PCM. OpenRouter publishes no per-model list of accepted containers —
  only a global set with "wav is the safest default" — and the provider that
  serves a given model documents its own subset (DeepInfra, which serves the
  Nemotron transcription models, documents just mp3 and wav). A provider that
  answers `unsupported or malformed audio` is therefore retried with the next
  encoding for that chunk; if all four are refused the error says the model
  cannot take the audio, which is a model problem rather than a file problem.
  The container that worked is saved as `TRANSCRIBE_AUDIO_FORMAT` and tried
  first next time.
- Before any re-encode happens, a refused chunk is re-sent unchanged in the
  plain (non-timestamped) response shape. OpenRouter's gateway reuses the one
  message above for "an audio option the routed provider does not support", so
  asking for `verbose_json` on a route that cannot produce timestamps looks
  exactly like corrupt audio — and costs four uploads to diagnose otherwise.
  When the plain shape works, the job continues with it and the log says the
  cue times were estimated from text length.
- The playback factor is an `atempo` chain (2× → `atempo=2`, 3× →
  `atempo=2,atempo=1.5`), applied while cutting the chunk, so no extra pass over
  the file is needed.
- Requests use OpenRouter's documented `input_audio` JSON shape with
  `verbose_json` segments, which is what the audio-transcription endpoint
  accepts for base64 audio. Fallbacks cover providers that answer with plain
  JSON or wrap the payload in `{"data": …}`.

## How ffmpeg is obtained

An existing install always wins; the program never downloads over one it can
use. In order, it looks at:

1. `FFMPEG_PATH`, if set.
2. `ffmpeg.exe` on `PATH`.
3. Next to the program, in `bin\`, and in `ffmpeg\bin\`.
4. `%LOCALAPPDATA%\cloudtranscribe\ffmpeg` (a copy this program downloaded
   earlier).
5. Common install directories: `C:\ffmpeg`, `C:\ffmpeg\bin`,
   `%ProgramFiles%\ffmpeg`, `%ProgramFiles%\ffmpeg\bin`,
   `%LOCALAPPDATA%\Programs\ffmpeg\bin`, `%LOCALAPPDATA%\Microsoft\WinGet\Links`,
   the scoop shims directory, and `C:\ProgramData\chocolatey\bin`.
6. Versioned package-manager folders, matched with a wildcard:
   `%LOCALAPPDATA%\Microsoft\WinGet\Packages\Gyan.FFmpeg*\ffmpeg*\bin`,
   `%LOCALAPPDATA%\Microsoft\WinGet\Packages\BtbN.FFmpeg*\ffmpeg*\bin`,
   `%USERPROFILE%\scoop\apps\ffmpeg*\current\bin`, and
   `%ProgramData%\chocolatey\lib\ffmpeg*\tools\ffmpeg*\bin`.
7. The folders video players are installed in, which is where ffmpeg.exe ends up
   when a guide names a VLC directory: VLC's own `InstallDir` from
   `HKCU`/`HKLM\Software\VideoLAN\VLC` (the lookup VLC does for itself),
   `%ProgramFiles%\VideoLAN\VLC`, the same under `Program Files (x86)`, and
   `%APPDATA%\vlc` with its `plugins` folder.

Only when none of those exists does it download a static build, trying three
hosts in order and keeping the first that answers:

| Host | Archive | Measured here |
| --- | --- | --- |
| gyan.dev build, mirrored on GitHub | ~118 MB | 5 MB/s |
| BtbN/FFmpeg-Builds (GitHub) | ~200 MB | 11 MB/s |
| www.gyan.dev directly | ~118 MB | 68 KB/s |

gyan.dev is listed on ffmpeg.org and is authoritative, but it throttles single
connections to tens of kilobytes per second — half an hour for the same file.
That is why the fast mirrors come first and the direct host last.

A source is abandoned instead of being waited on: after 30 s with an average
rate under 120 KB/s (too slow), or after 30 s with no bytes at all (stalled).
The next source is tried and the failure is logged. The archive is written to
`ffmpeg-download.zip` in the install folder so a slow transfer is visible in
Explorer, and it is deleted once `ffmpeg.exe` and `ffprobe.exe` are extracted.

Transcribed media never touches the disk: chunks are cut and uploaded from
memory.
