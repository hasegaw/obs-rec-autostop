package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hasegaw/obs_rec_autostop/internal/config"
	"github.com/hasegaw/obs_rec_autostop/internal/monitor"
	"github.com/hasegaw/obs_rec_autostop/internal/obs"
)

const silentMeter = `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0,0,0.9]]}]}`
const loudMeter = `{"inputs":[{"inputName":"Mic","inputLevelsMul":[[0.1,0.5,0.9]]}]}`

type fakeScenario struct {
	idle            bool
	noMeters        bool
	missingInput    bool
	noStopSupport   bool
	disconnect      bool
	loudDuringCheck bool
	failStop        bool
}

type fakeStats struct {
	stops     atomic.Int32
	checks    atomic.Int32
	loudAt    atomic.Int64
	stoppedAt atomic.Int64
}

// fakeOBS exercises the actual WebSocket transport and app, never a real OBS.
func fakeOBS(t *testing.T, scenario fakeScenario) (string, *fakeStats) {
	t.Helper()
	stats := &fakeStats{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		send := func(op int, data any) error {
			return conn.WriteJSON(map[string]any{"op": op, "d": data})
		}
		if send(0, map[string]any{"rpcVersion": 1}) != nil {
			return
		}
		var identify struct {
			Op   int `json:"op"`
			Data struct {
				Subscriptions int `json:"eventSubscriptions"`
			} `json:"d"`
		}
		if conn.ReadJSON(&identify) != nil {
			return
		}
		if identify.Op != 1 || identify.Data.Subscriptions != subscriptions {
			t.Errorf("invalid Identify: %+v", identify)
			return
		}
		if send(2, map[string]any{"negotiatedRpcVersion": 1}) != nil {
			return
		}
		type request struct {
			Op   int `json:"op"`
			Data struct {
				Type string `json:"requestType"`
				ID   string `json:"requestId"`
			} `json:"d"`
		}
		requests := make(chan request)
		closed := make(chan struct{})
		defer close(closed)
		go func() {
			defer close(requests)
			for {
				var req request
				if conn.ReadJSON(&req) != nil {
					return
				}
				select {
				case requests <- req:
				case <-closed:
					return
				}
			}
		}()
		event := func(kind string, data any) error {
			return send(5, map[string]any{"eventType": kind, "eventData": data})
		}
		active, initialized, ticks := !scenario.idle, false, 0
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case req, ok := <-requests:
				if !ok {
					return
				}
				var data any
				result, code := true, 100
				switch req.Data.Type {
				case "GetVersion":
					available := []string{"GetVersion", "GetInputList", "GetRecordStatus"}
					if !scenario.noStopSupport {
						available = append(available, "StopRecord")
					}
					data = map[string]any{"obsVersion": "30.0", "obsWebSocketVersion": "5.0", "availableRequests": available}
				case "GetInputList":
					data = map[string]any{"inputs": []any{map[string]any{"inputName": "Mic", "inputKind": "test_audio"}}}
				case "GetRecordStatus":
					n := stats.checks.Add(1)
					if scenario.loudDuringCheck && n == 2 {
						stats.loudAt.Store(time.Now().UnixNano())
						if event("InputVolumeMeters", json.RawMessage(loudMeter)) != nil {
							return
						}
					}
					data = recordStatus{Active: active}
					initialized = true
				case "StopRecord":
					stats.stops.Add(1)
					stats.stoppedAt.Store(time.Now().UnixNano())
					data = map[string]any{"outputPath": "/test/recording.mkv"}
					if scenario.failStop {
						result, code = false, 500
					}
				default:
					t.Errorf("unexpected request %q", req.Data.Type)
					return
				}
				if send(7, map[string]any{"requestType": req.Data.Type, "requestId": req.Data.ID, "requestStatus": map[string]any{"result": result, "code": code}, "responseData": data}) != nil {
					return
				}
			case <-ticker.C:
				if !initialized {
					continue
				}
				ticks++
				if scenario.disconnect && ticks >= 2 {
					return
				}
				if !active {
					if ticks < 3 {
						continue
					}
					active = true
					if event("RecordStateChanged", map[string]any{"outputState": "OBS_WEBSOCKET_OUTPUT_STARTED", "outputActive": true}) != nil {
						return
					}
				}
				if scenario.noMeters {
					continue
				}
				meter := silentMeter
				if scenario.missingInput {
					meter = `{"inputs":[]}`
				}
				if event("InputVolumeMeters", json.RawMessage(meter)) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), stats
}

func testConfig(url string) config.Config {
	return config.Config{URL: url, SilenceDuration: 100 * time.Millisecond, ThresholdDB: -50, Inputs: []string{"Mic"}, MeterTimeout: 300 * time.Millisecond, RequestTimeout: time.Second}
}

func TestRunAgainstFakeOBS(t *testing.T) {
	for _, tt := range []struct {
		name      string
		scenario  fakeScenario
		opts      options
		wantStops int32
		wantErr   string
		wantLog   string
	}{
		{"stop recording", fakeScenario{}, options{}, 1, "", "Recording stopped"},
		{"wait for start", fakeScenario{idle: true}, options{}, 1, "", "Waiting for recording to start"},
		{"dry run", fakeScenario{}, options{DryRun: true}, 0, "", "DRY RUN"},
		{"no samples", fakeScenario{noMeters: true}, options{}, 0, "audio meters unavailable", ""},
		{"missing source", fakeScenario{missingInput: true}, options{}, 0, "audio meters unavailable", ""},
		{"unsupported control", fakeScenario{noStopSupport: true}, options{}, 0, "StopRecord is unavailable", ""},
		{"connection lost", fakeScenario{disconnect: true}, options{}, 0, "connection lost", ""},
		{"list inputs", fakeScenario{}, options{ListInputs: true}, 0, "", "Connection OK"},
		{"level check", fakeScenario{}, options{CheckLevels: 100 * time.Millisecond}, 0, "", "Audio level check complete"},
		{"level check missing meters", fakeScenario{noMeters: true}, options{CheckLevels: 100 * time.Millisecond}, 0, "no usable samples", ""},
		{"stop rejected", fakeScenario{failStop: true}, options{}, 1, "StopRecord failed", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			url, stats := fakeOBS(t, tt.scenario)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var logs, out bytes.Buffer
			err := run(ctx, testConfig(url), tt.opts, log.New(&logs, "", 0), &out)
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error=%v; wanted %q; logs=%s", err, tt.wantErr, logs.String())
			}
			if got := stats.stops.Load(); got != tt.wantStops {
				t.Fatalf("StopRecord count=%d; want %d", got, tt.wantStops)
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Fatalf("missing log %q in %s", tt.wantLog, logs.String())
			}
			if tt.opts.ListInputs && !strings.Contains(out.String(), `"Mic"`) {
				t.Fatalf("unexpected input list: %s", &out)
			}
		})
	}
}

func TestSoundDuringFinalStatusCheckResetsSilence(t *testing.T) {
	url, stats := fakeOBS(t, fakeScenario{loudDuringCheck: true})
	cfg := testConfig(url)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := run(ctx, cfg, options{}, log.New(io.Discard, "", 0), io.Discard); err != nil {
		t.Fatal(err)
	}
	if stats.stops.Load() != 1 || stats.checks.Load() < 3 {
		t.Fatalf("stops=%d, status checks=%d", stats.stops.Load(), stats.checks.Load())
	}
	if elapsed := time.Duration(stats.stoppedAt.Load() - stats.loudAt.Load()); elapsed < cfg.SilenceDuration {
		t.Fatalf("stopped only %s after audio resumed", elapsed)
	}
}

func TestPauseResumeAndManualStop(t *testing.T) {
	t0 := time.Unix(100, 0)
	cfg := testConfig("")
	state := watchState{cfg: cfg, logger: log.New(io.Discard, "", 0), started: t0, detector: monitor.Detector{Duration: time.Second, MaxGap: 2 * time.Second, Threshold: 0.01}}
	state.status(recordStatus{Active: true}, t0)
	meter := func(at time.Time) {
		t.Helper()
		if err := state.event(obs.Event{Type: "InputVolumeMeters", Data: json.RawMessage(silentMeter), ReceivedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	change := func(name string, at time.Time) {
		t.Helper()
		data := json.RawMessage(fmt.Sprintf(`{"outputState":%q,"outputActive":false}`, "OBS_WEBSOCKET_OUTPUT_"+name))
		if err := state.event(obs.Event{Type: "RecordStateChanged", Data: data, ReceivedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	meter(t0)
	meter(t0.Add(time.Second))
	if !state.detector.Ready(t0.Add(time.Second)) {
		t.Fatal("expected initial silence")
	}
	change("PAUSED", t0.Add(time.Second))
	meter(t0.Add(2 * time.Second))
	if state.finished || !state.paused || state.detector.Ready(t0.Add(2*time.Second)) {
		t.Fatal("pause must reset and wait, not end recording")
	}
	change("RESUMED", t0.Add(3*time.Second))
	meter(t0.Add(3 * time.Second))
	if state.detector.Ready(t0.Add(3 * time.Second)) {
		t.Fatal("paused time must not count")
	}
	meter(t0.Add(4 * time.Second))
	if !state.detector.Ready(t0.Add(4 * time.Second)) {
		t.Fatal("silence after resume should count")
	}
	change("STOPPED", t0.Add(4*time.Second))
	if !state.finished {
		t.Fatal("manual stop must exit")
	}
}
