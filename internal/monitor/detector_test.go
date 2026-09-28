package monitor

import (
	"math"
	"testing"
	"time"
)

func TestContinuousSilence(t *testing.T) {
	t0 := time.Unix(100, 0)
	d := Detector{Duration: 10 * time.Second, MaxGap: 2 * time.Second, Threshold: 0.01}
	for i := 0; i <= 10; i++ {
		d.Add(t0.Add(time.Duration(i)*time.Second), 0.01)
		if got := d.Ready(t0.Add(time.Duration(i) * time.Second)); got != (i == 10) {
			t.Fatalf("second %d: ready=%v", i, got)
		}
	}
	if d.Ready(t0.Add(13 * time.Second)) {
		t.Fatal("stale samples must not cause a stop")
	}
	d.Add(t0.Add(11*time.Second), 0.011)
	if d.Ready(t0.Add(11*time.Second)) || d.Elapsed() != 0 {
		t.Fatal("sound must reset the full silence period")
	}
}

func TestMissingSamplesDoNotCount(t *testing.T) {
	t0 := time.Unix(100, 0)
	d := Detector{Duration: 10 * time.Second, MaxGap: 2 * time.Second, Threshold: 0.01}
	d.Add(t0, 0)
	if d.Ready(t0.Add(10 * time.Second)) {
		t.Fatal("wall time without new samples is not silence")
	}
	d.Add(t0.Add(20*time.Second), 0)
	if d.Elapsed() != 0 || d.Ready(t0.Add(20*time.Second)) {
		t.Fatal("gap must reset the timer")
	}
	d.Add(t0.Add(21*time.Second), math.NaN())
	if d.Elapsed() != 0 {
		t.Fatal("invalid sample must reset the timer")
	}
}

func TestPeak(t *testing.T) {
	for _, tt := range []struct {
		name     string
		data     string
		selected []string
		want     float64
		bad      bool
	}{
		{"post-mute", `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0,0,0.9],[0,0,1]]}]}`, nil, 0, false},
		{"all channels", `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0.1,0.2,1],[0.1,0.7,1]]},{"inputName":"Music","inputLevelsMul":[[0,0.9,1]]}]}`, nil, 0.9, false},
		{"selection", `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0,0,1]]},{"inputName":"Music","inputLevelsMul":[[0,0.9,1]]}]}`, []string{"Mic"}, 0, false},
		{"missing selection", `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0,0,1]]}]}`, []string{"Mic", "Music"}, 0, true},
		{"empty", `{"inputs":[]}`, nil, 0, true},
		{"empty channels", `{"inputs":[{"inputName":"Mic","inputLevelsMul":[]}]}`, nil, 0, true},
		{"malformed", `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0]]}]}`, nil, 0, true},
		{"negative", `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0,-1,0]]}]}`, nil, 0, true},
		{"null", `null`, nil, 0, true},
		{"null peak", `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0,null,0]]}]}`, nil, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Peak([]byte(tt.data), tt.selected)
			if (err != nil) != tt.bad || got != tt.want {
				t.Fatalf("Peak=%v, err=%v; want %v, bad=%v", got, err, tt.want, tt.bad)
			}
		})
	}
}
