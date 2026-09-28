package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"time"

	"github.com/hasegaw/obs_rec_autostop/internal/config"
	"github.com/hasegaw/obs_rec_autostop/internal/monitor"
	"github.com/hasegaw/obs_rec_autostop/internal/obs"
)

const subscriptions = (1 << 6) | (1 << 16) // Outputs | InputVolumeMeters

type options struct {
	DryRun      bool
	ListInputs  bool
	CheckLevels time.Duration
}

type recordStatus struct {
	Active bool `json:"outputActive"`
	Paused bool `json:"outputPaused"`
}

func run(ctx context.Context, cfg config.Config, opts options, logger *log.Logger, out io.Writer) error {
	logger.Print("Checking OBS WebSocket connection and authentication...")
	dialCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	client, err := obs.Dial(dialCtx, cfg.URL, cfg.Password, subscriptions)
	cancel()
	if err != nil {
		return fmt.Errorf("OBS connection test failed: %w", err)
	}
	defer client.Close()
	call := func(name string, result any) error {
		reqCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
		defer cancel()
		return client.Call(reqCtx, name, nil, result)
	}

	var version struct {
		OBS       string   `json:"obsVersion"`
		WebSocket string   `json:"obsWebSocketVersion"`
		Requests  []string `json:"availableRequests"`
	}
	if err := call("GetVersion", &version); err != nil {
		return fmt.Errorf("OBS connection test (GetVersion): %w", err)
	}
	for _, required := range []string{"GetInputList", "GetRecordStatus", "StopRecord"} {
		found := false
		for _, available := range version.Requests {
			found = found || available == required
		}
		if !found {
			return fmt.Errorf("OBS connection test: required request %s is unavailable", required)
		}
	}
	var inputs struct {
		Inputs []struct {
			Name string `json:"inputName"`
			Kind string `json:"inputKind"`
		} `json:"inputs"`
	}
	if err := call("GetInputList", &inputs); err != nil {
		return fmt.Errorf("OBS connection test (GetInputList): %w", err)
	}
	var initial recordStatus
	if err := call("GetRecordStatus", &initial); err != nil {
		return fmt.Errorf("OBS connection test (GetRecordStatus): %w", err)
	}
	logger.Printf("Connection OK: OBS %s / WebSocket %s / recording=%t / paused=%t / StopRecord supported", version.OBS, version.WebSocket, initial.Active, initial.Paused)
	if opts.ListInputs {
		for _, input := range inputs.Inputs {
			fmt.Fprintf(out, "%q\t%s\n", input.Name, input.Kind)
		}
		return nil
	}
	for _, selected := range cfg.Inputs {
		found := false
		for _, input := range inputs.Inputs {
			found = found || selected == input.Name
		}
		if !found {
			return fmt.Errorf("OBS audio input %q does not exist; check -list-inputs", selected)
		}
	}
	if opts.CheckLevels > 0 {
		return checkLevels(ctx, client, cfg, opts.CheckLevels, logger)
	}
	logger.Printf("Silence condition: %s continuously at or below %.1f dBFS / dry-run=%t", cfg.SilenceDuration, cfg.ThresholdDB, opts.DryRun)
	if len(cfg.Inputs) == 0 {
		logger.Print("Monitoring all active audio inputs.")
	} else {
		logger.Printf("Monitoring inputs: %q", cfg.Inputs)
	}

	started := time.Now()
	state := watchState{
		cfg: cfg, logger: logger, started: started,
		detector: monitor.Detector{Duration: cfg.SilenceDuration, MaxGap: cfg.MeterTimeout, Threshold: math.Pow(10, cfg.ThresholdDB/20)},
	}
	state.status(initial, started)
	if !state.active {
		logger.Print("Waiting for recording to start.")
	}
	interval := min(time.Second, cfg.MeterTimeout/2)
	if interval <= 0 {
		interval = time.Nanosecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastPoll, lastProgress := started, started

	// Process messages accumulated during an RPC before making a stop decision.
	// A loud sample, pause, or recording restart invalidates earlier silence.
	drain := func() error {
		for {
			select {
			case event, ok := <-client.Events():
				if !ok {
					return fmt.Errorf("OBS connection lost: %w", client.Err())
				}
				if err := state.event(event); err != nil {
					return err
				}
			default:
				return nil
			}
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-client.Done():
			return fmt.Errorf("OBS connection lost: %w", client.Err())
		case event, ok := <-client.Events():
			if !ok {
				return fmt.Errorf("OBS connection lost: %w", client.Err())
			}
			if err := state.event(event); err != nil {
				return err
			}
		case <-ticker.C:
			if time.Since(lastPoll) >= 2*time.Second {
				var current recordStatus
				if err := call("GetRecordStatus", &current); err != nil {
					return fmt.Errorf("recording status check: %w", err)
				}
				state.status(current, time.Now())
				lastPoll = time.Now()
			}
		}
		if err := drain(); err != nil {
			return err
		}
		if state.finished {
			logger.Print("OBS recording has ended; exiting.")
			return nil
		}
		if !state.active || state.paused {
			continue
		}
		if time.Since(state.lastValid) > cfg.MeterTimeout {
			return fmt.Errorf("usable audio meters unavailable for %s (%s); recording was not stopped", cfg.MeterTimeout, state.meterProblem)
		}
		if time.Since(lastProgress) >= 30*time.Second {
			logger.Printf("Monitoring: continuous silence %s / %s", state.detector.Elapsed().Truncate(time.Second), cfg.SilenceDuration)
			lastProgress = time.Now()
		}
		if !state.detector.Ready(time.Now()) {
			continue
		}

		// Verify that recording is still running before requesting its stop.
		var current recordStatus
		if err := call("GetRecordStatus", &current); err != nil {
			return fmt.Errorf("pre-stop recording status check: %w", err)
		}
		state.status(current, time.Now())
		if err := drain(); err != nil {
			return err
		}
		if state.finished {
			logger.Print("OBS recording has already ended.")
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := client.Err(); err != nil {
			return fmt.Errorf("OBS connection lost before stop: %w", err)
		}
		if !state.active || state.paused || !state.detector.Ready(time.Now()) {
			continue
		}
		if opts.DryRun {
			logger.Printf("DRY RUN: Detected %s of silence. Exiting without stopping recording.", cfg.SilenceDuration)
			return nil
		}
		logger.Printf("Detected %s of silence. Stopping OBS recording.", cfg.SilenceDuration)
		var stopped struct {
			Path string `json:"outputPath"`
		}
		if err := call("StopRecord", &stopped); err != nil {
			return fmt.Errorf("StopRecord failed or its result could not be confirmed (not retried): %w", err)
		}
		logger.Printf("Recording stopped: %s", stopped.Path)
		return nil
	}
}

type watchState struct {
	cfg          config.Config
	logger       *log.Logger
	detector     monitor.Detector
	started      time.Time
	active       bool
	paused       bool
	seen         bool
	finished     bool
	lastValid    time.Time
	meterProblem string
}

func (s *watchState) reset(at time.Time) {
	s.detector.Reset()
	s.lastValid = at
	s.meterProblem = "waiting for audio meters"
}

func (s *watchState) status(status recordStatus, at time.Time) {
	active := status.Active || status.Paused
	if active && (!s.active || status.Paused != s.paused) {
		s.reset(at)
		if status.Paused {
			s.logger.Print("Recording is paused. Silence timer reset.")
		} else {
			s.logger.Print("Recording is active. Starting audio monitoring.")
		}
	}
	s.active, s.paused = active, status.Paused
	if active {
		s.seen = true
	} else {
		s.detector.Reset()
		if s.seen {
			s.finished = true
		}
	}
}

func (s *watchState) event(event obs.Event) error {
	if event.ReceivedAt.Before(s.started) {
		return nil
	}
	switch event.Type {
	case "RecordStateChanged":
		var data struct {
			State string `json:"outputState"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return fmt.Errorf("invalid recording-state event")
		}
		switch data.State {
		case "OBS_WEBSOCKET_OUTPUT_STARTED", "OBS_WEBSOCKET_OUTPUT_RESUMED":
			s.reset(event.ReceivedAt)
			s.status(recordStatus{Active: true}, event.ReceivedAt)
		case "OBS_WEBSOCKET_OUTPUT_PAUSED":
			// OBS sends outputActive=false for PAUSED; use the state enum.
			s.status(recordStatus{Active: true, Paused: true}, event.ReceivedAt)
		case "OBS_WEBSOCKET_OUTPUT_STOPPING", "OBS_WEBSOCKET_OUTPUT_STOPPED":
			s.status(recordStatus{}, event.ReceivedAt)
		case "OBS_WEBSOCKET_OUTPUT_STARTING":
			s.reset(event.ReceivedAt)
		default:
			return fmt.Errorf("unsupported recording state %q", data.State)
		}
	case "InputVolumeMeters":
		if !s.active || s.paused || s.finished {
			return nil
		}
		peak, err := monitor.Peak(event.Data, s.cfg.Inputs)
		if err != nil {
			s.detector.Reset()
			if s.meterProblem != err.Error() {
				s.logger.Printf("Waiting for audio meters: %v (silence timer reset)", err)
			}
			s.meterProblem = err.Error()
			return nil
		}
		if s.meterProblem != "" {
			s.logger.Print("Receiving audio meters.")
		}
		s.meterProblem = ""
		s.lastValid = event.ReceivedAt
		s.detector.Add(event.ReceivedAt, peak)
	}
	return nil
}

// checkLevels is a read-only diagnostic; it never sends StopRecord.
func checkLevels(ctx context.Context, client *obs.Client, cfg config.Config, duration time.Duration, logger *log.Logger) error {
	logger.Printf("Checking audio levels for %s / threshold %.1f dBFS (recording will not be changed)", duration, cfg.ThresholdDB)
	started := time.Now()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	watchdog := time.NewTicker(min(time.Second, max(time.Nanosecond, cfg.MeterTimeout/2)))
	defer watchdog.Stop()
	lastValid, lastPrint := started, time.Time{}
	count, audible := 0, 0
	minDB, maxDB := math.Inf(1), math.Inf(-1)
	problem := "no audio meter events"
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-client.Done():
			return fmt.Errorf("audio-level check: %w", client.Err())
		case <-watchdog.C:
			if time.Since(lastValid) > cfg.MeterTimeout {
				return fmt.Errorf("audio-level check: no usable meters for %s (%s)", cfg.MeterTimeout, problem)
			}
		case <-timer.C:
			if count == 0 {
				return fmt.Errorf("audio-level check: no usable samples (%s)", problem)
			}
			logger.Printf("Audio level check complete: %d samples / peak range %.1f to %.1f dBFS / above threshold %d samples (%.1f%%)", count, minDB, maxDB, audible, 100*float64(audible)/float64(count))
			return nil
		case event, ok := <-client.Events():
			if !ok {
				return fmt.Errorf("audio-level check: %w", client.Err())
			}
			if event.Type != "InputVolumeMeters" || event.ReceivedAt.Before(started) {
				continue
			}
			peak, err := monitor.Peak(event.Data, cfg.Inputs)
			if err != nil {
				problem = err.Error()
				continue
			}
			lastValid = event.ReceivedAt
			count++
			db := 20 * math.Log10(peak)
			minDB, maxDB = math.Min(minDB, db), math.Max(maxDB, db)
			label := "silent"
			if db > cfg.ThresholdDB {
				audible++
				label = "audible"
			}
			if event.ReceivedAt.Sub(lastPrint) >= time.Second {
				logger.Printf("Audio peak: %.1f dBFS / %s", db, label)
				lastPrint = event.ReceivedAt
			}
		}
	}
}
