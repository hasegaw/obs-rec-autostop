# obs-rec-autostop

A Go CLI that stops an OBS recording through OBS WebSocket after audio levels remain at or below a threshold for 10 consecutive minutes. If OBS is already recording when the tool starts, monitoring begins immediately. Otherwise, it waits for a recording to start.

## Requirements

- OBS Studio 28 or later with OBS WebSocket v5. OBS WebSocket is built into OBS 28 and later. [Official OBS guide](https://obsproject.com/kb/remote-control-guide)
- Go 1.23 or later to build from source. Go is not required to run a compiled binary.

## Releases and automated builds

Download and extract the archive for your platform from GitHub Releases. Go is not required to run release binaries. Each archive contains the executable, `README.md`, `.env.example`, and `LICENSE`.

| Release file (example: `v0.1.0`) | Platform and contents |
| --- | --- |
| `obs-rec-autostop_v0.1.0_macos_arm64.tar.gz` | `obs-rec-autostop` for Apple Silicon Macs |
| `obs-rec-autostop_v0.1.0_macos_amd64.tar.gz` | `obs-rec-autostop` for Intel Macs |
| `obs-rec-autostop_v0.1.0_windows_amd64.zip` | `obs-rec-autostop.exe` for Windows on Intel/AMD processors |
| `obs-rec-autostop_v0.1.0_windows_arm64.zip` | `obs-rec-autostop.exe` for Windows on ARM |
| `checksums.txt` | SHA-256 checksums for the four archives above |

On Windows, extract the ZIP for your platform and run `./obs-rec-autostop.exe` from that folder. Copy `.env.example` to `.env` and enter your connection settings first. Archives include only the configuration template; the development environment's `.env` is never included.

The `.github/workflows/release.yml` workflow runs when you push a tag such as `v0.1.0`. It runs tests and `go vet` on macOS and Windows, then builds all four binaries and publishes the four archives and checksums to a GitHub Release. Release binaries are built with `CGO_ENABLED=0`. The workflow does not include code signing or notarization.

Commit and push the code, including the workflow, to your GitHub repository, then tag the commit you want to release:

```sh
git tag v0.1.0
git push origin v0.1.0
```

Use the tag format `vX.Y.Z`. Tags such as `v0.1.0-rc.1` create prereleases. The workflow uses Actions' built-in `GITHUB_TOKEN` with `contents: write` permission, so no additional token is needed. If the workflow fails before publication, rerun it to upload the assets to the draft again. Published releases are not overwritten; use a new tag for the next version.

To generate the same release archives locally, install Go and run the following command from the repository root. It saves the files to `dist/v0.1.0/` without publishing anything to GitHub.

```sh
go run ./cmd/build-release v0.1.0
```

## Setup

1. In OBS, open **Tools > WebSocket Server Settings**, enable the server, and check its port and password.
2. Build the tool in this directory:

   ```sh
   CGO_ENABLED=0 go build -o obs-rec-autostop .
   ```

3. Copy the configuration template, then edit the connection URL and password in `.env`:

   ```sh
   cp .env.example .env
   ```

4. Check the connection and input names:

   ```sh
   ./obs-rec-autostop -list-inputs
   ```

   This list includes non-audio OBS inputs such as video and images. To monitor specific audio inputs, find their names in the OBS Audio Mixer and set `OBS_AUDIO_INPUTS` in `.env`. Names must exactly match the input names in your OBS configuration; replace the examples below as needed.

   ```dotenv
   OBS_AUDIO_INPUTS='["Desktop Audio","Mic/Aux"]'
   ```

5. To check audio levels before recording, play audio through the inputs you want to monitor and run:

   ```sh
   ./obs-rec-autostop -check-levels 10s
   ```

   After checking the connection, the tool reads meters for 10 seconds and exits, whether or not OBS is recording. It does not call `StopRecord`.

6. Start a recording in OBS, then run the tool:

   ```sh
   ./obs-rec-autostop
   ```

At startup, the tool checks authentication, support for the required APIs, the input list, and the recording state. It logs a successful connection check before monitoring or waiting. If OBS is not recording at startup, the tool waits for a recording to start. It never starts a recording itself.

## Configuration

The tool reads `.env` from the current working directory. Settings take precedence in this order: explicit CLI options, process environment variables, `.env`, then defaults. If the default `.env` file does not exist, the tool can still start using environment variables or defaults.

| Environment variable | Default | Description |
| --- | --- | --- |
| `OBS_WS_URL` | `ws://127.0.0.1:4455` | OBS WebSocket URL; use `ws://` or `wss://` |
| `OBS_WS_PASSWORD` | Empty string | Password configured in OBS |
| `SILENCE_DURATION` | `10m` | Continuous silence required before stopping; a positive duration such as `30s` or `10m` |
| `SILENCE_THRESHOLD_DB` | `-50` | Peak level at or below which audio is considered silent; a finite dBFS value at or below 0 |
| `OBS_AUDIO_INPUTS` | `[]` | JSON array of input names to monitor; an empty array monitors all active audio inputs |
| `METER_TIMEOUT` | `5s` | How long the tool tolerates receiving no valid audio meters |
| `REQUEST_TIMEOUT` | `10s` | Timeout for requests to OBS |

The silence timer advances only while the peaks of all monitored inputs and channels remain at or below the threshold. For example, `-50` can treat quiet background noise as silence. A lower value such as `-60` allows quieter sounds to reset the timer. Adjust this setting for your recording environment.

With `OBS_AUDIO_INPUTS=[]`, active background music or audio that is not part of the recording may affect detection. Specify input names when you know which inputs you want to record. All specified inputs must appear in the audio meters. Misspelled names, deleted inputs, or inputs deactivated by a scene change are treated as missing meters.

`.env` and `.env.*` are excluded from Git, except for the distributed `.env.example` template.

## CLI

| Option | Behavior |
| --- | --- |
| `-env PATH` | Select a configuration file; defaults to `.env`. Use `-env ''` to disable file loading |
| `-silence-duration 30s` | Override the silence duration for this run |
| `-dry-run` | Log when the stop condition is met, then exit without calling `StopRecord` |
| `-list-inputs` | Check the connection, list all OBS inputs, then exit |
| `-check-levels 10s` | Check the connection, read audio meters for the specified duration, then exit. Works while OBS is not recording and does not call `StopRecord` |
| `-h` | Show help |

```sh
./obs-rec-autostop -env /path/to/obs.env
./obs-rec-autostop -env ''
./obs-rec-autostop -check-levels 10s
./obs-rec-autostop -dry-run -silence-duration 10s
```

On Windows, the executable is named `obs-rec-autostop.exe`; run it as `./obs-rec-autostop.exe` in PowerShell. To build with CGO disabled in PowerShell, set `$env:CGO_ENABLED = "0"`, then run `go build -o obs-rec-autostop.exe .`.

## Detection and exit behavior

- Silence is measured using valid meters received after monitoring begins. Silence before startup does not count.
- Audio above the threshold resets the timer. With the default duration, silence must last for 10 consecutive minutes, rather than 10 minutes in total.
- Pausing a recording resets the timer. Timing starts again after recording resumes.
- When the stop condition is met, the tool checks the recording state again, calls `StopRecord`, and exits after the request succeeds. It also exits if you stop the recording manually.
- Missing meters reset the silence timer. If meters remain missing for `METER_TIMEOUT`, the connection drops, or authentication fails, the tool exits with an error. It does not reconnect automatically or stop the recording because of these errors.
- `Ctrl+C` exits the tool without changing the OBS recording state.

OBS normally sends meters for active audio inputs about every 50 ms. For each channel, the tool uses the peak value that reflects volume and mute settings (`inputLevelsMul[channel][1]`). [Event specification](https://github.com/obsproject/obs-websocket/blob/master/docs/generated/protocol.md#inputvolumemeters), [meter value generation](https://github.com/obsproject/obs-websocket/blob/master/src/utils/Obs_VolumeMeter.cpp#L60-L91)

## Checking your setup

1. Run `./obs-rec-autostop -list-inputs` to check the connection.
2. Play audio through the monitored inputs and run `./obs-rec-autostop -check-levels 10s` to check the levels. This step does not require a recording.
3. Start a test recording in OBS and run `./obs-rec-autostop -dry-run -silence-duration 10s`.
4. Play audio through the monitored inputs, then play it again before silence reaches 10 seconds. Check that the timer resets and that the stop condition is logged after another 10 seconds of silence. The recording continues.
5. Restart the tool with `./obs-rec-autostop -silence-duration 10s`. Check that 10 seconds of silence stops the test recording and saves the file.
6. Also check recording pause/resume and `Ctrl+C` before using the tool normally without overriding the duration.

## Detection limits

The tool does not use speech recognition or voice activity detection (VAD) to determine whether someone is speaking. Background music, machine noise, and ambient sounds all count toward the audio level.

The tool reads audio levels for individual inputs; it does not analyze the recording file or recording tracks. Check the track assignments and any **Monitor Only** settings, and select the inputs you want to record. OBS also sends meter data for inputs set to **Monitor Only**. [OBS audio processing](https://github.com/obsproject/obs-studio/blob/master/libobs/obs-source.c#L1573-L1582)

The tool can detect missing WebSocket events. However, if OBS reports a device failure or a stopped audio stream as a zero level, the tool cannot distinguish it from actual silence. [Meter reset behavior](https://github.com/obsproject/obs-websocket/blob/master/src/utils/Obs_VolumeMeter.cpp#L75-L100)

## Development checks

```sh
go test ./...
go vet ./...
```

## License

This project is licensed under the [MIT License](LICENSE). Copyright (c) 2026 Takeshi HASEGAWA.
