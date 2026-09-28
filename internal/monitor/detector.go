// Package monitor evaluates confirmed, continuous audio-meter samples.
package monitor

import (
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Peak returns the loudest post-fader, post-mute channel in the selected inputs.
// Missing inputs are unavailable data, never evidence of silence.
func Peak(data json.RawMessage, selected []string) (float64, error) {
	var meters struct {
		Inputs []struct {
			Name   string       `json:"inputName"`
			Levels [][]*float64 `json:"inputLevelsMul"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(data, &meters); err != nil {
		return 0, fmt.Errorf("invalid volume-meter event")
	}
	wanted := make(map[string]bool, len(selected))
	for _, name := range selected {
		wanted[name] = false
	}
	peak, channels := 0.0, 0
	for _, input := range meters.Inputs {
		if len(wanted) > 0 {
			if _, ok := wanted[input.Name]; !ok {
				continue
			}
		}
		if len(input.Levels) == 0 {
			return 0, fmt.Errorf("audio input %q has no meter channels", input.Name)
		}
		for _, level := range input.Levels {
			// OBS sends [RMS, peak, raw peak]. Raw peak ignores gain and mute.
			if len(level) < 2 || level[1] == nil || math.IsNaN(*level[1]) || math.IsInf(*level[1], 0) || *level[1] < 0 {
				return 0, fmt.Errorf("audio input %q has an invalid peak", input.Name)
			}
			peak = math.Max(peak, *level[1])
			channels++
		}
		if len(wanted) > 0 {
			wanted[input.Name] = true
		}
	}
	for _, name := range selected {
		if !wanted[name] {
			return 0, fmt.Errorf("audio input %q is absent from active meters", name)
		}
	}
	if channels == 0 {
		return 0, fmt.Errorf("no active audio meters")
	}
	return peak, nil
}

// Detector uses sample timestamps, so waiting without samples cannot stop OBS.
type Detector struct {
	Duration  time.Duration
	MaxGap    time.Duration
	Threshold float64
	since     time.Time
	last      time.Time
}

func (d *Detector) Reset() {
	d.since = time.Time{}
	d.last = time.Time{}
}

func (d *Detector) Add(at time.Time, peak float64) {
	if math.IsNaN(peak) || math.IsInf(peak, 0) || peak < 0 {
		d.Reset()
		return
	}
	if !d.last.IsZero() && (!at.After(d.last) || at.Sub(d.last) > d.MaxGap) {
		d.Reset()
	}
	d.last = at
	if peak > d.Threshold {
		d.since = time.Time{}
		return
	}
	if d.since.IsZero() {
		d.since = at
	}
}

func (d *Detector) Ready(now time.Time) bool {
	return !d.since.IsZero() && !now.Before(d.last) && now.Sub(d.last) <= d.MaxGap && d.last.Sub(d.since) >= d.Duration
}

func (d *Detector) Elapsed() time.Duration {
	if d.since.IsZero() {
		return 0
	}
	return d.last.Sub(d.since)
}
