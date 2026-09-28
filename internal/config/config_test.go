package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var configKeys = []string{
	"OBS_WS_URL", "OBS_WS_PASSWORD", "SILENCE_DURATION", "SILENCE_THRESHOLD_DB",
	"OBS_AUDIO_INPUTS", "METER_TIMEOUT", "REQUEST_TIMEOUT",
}

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range configKeys {
		value, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func writeEnv(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.env")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaults(t *testing.T) {
	cleanEnvironment(t)
	got, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		URL: "ws://127.0.0.1:4455", SilenceDuration: 10 * time.Minute,
		ThresholdDB: -50, Inputs: []string{}, MeterTimeout: 5 * time.Second,
		RequestTimeout: 10 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config = %#v, want %#v", got, want)
	}
}

func TestFileSettingsAndEnvironmentPrecedence(t *testing.T) {
	cleanEnvironment(t)
	path := writeEnv(t, `OBS_WS_URL=wss://localhost:4456/obs
OBS_WS_PASSWORD=file-password
SILENCE_DURATION=20m
SILENCE_THRESHOLD_DB=-45.5
OBS_AUDIO_INPUTS='["マイク", "Desktop Audio"]'
METER_TIMEOUT=8s
REQUEST_TIMEOUT=3s
`)
	t.Setenv("OBS_WS_PASSWORD", "")
	t.Setenv("SILENCE_DURATION", "30s")
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		URL: "wss://localhost:4456/obs", Password: "", SilenceDuration: 30 * time.Second,
		ThresholdDB: -45.5, Inputs: []string{"マイク", "Desktop Audio"},
		MeterTimeout: 8 * time.Second, RequestTimeout: 3 * time.Second,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("config = %#v, want %#v", got, want)
	}
	if _, exists := os.LookupEnv("OBS_WS_URL"); exists {
		t.Error("Load modified the process environment")
	}
}

func TestMissingEnvironmentFiles(t *testing.T) {
	cleanEnvironment(t)
	// The default filename is optional even if the working directory has no file.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if _, err := Load(".env"); err != nil {
		t.Fatalf("missing default file: %v", err)
	}
	if _, err := Load("custom.env"); err == nil {
		t.Error("missing explicitly selected file must fail")
	}
}

func TestInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"SILENCE_DURATION", "0s"}, {"SILENCE_DURATION", "-1m"}, {"SILENCE_DURATION", "10"},
		{"SILENCE_DURATION", ""}, {"METER_TIMEOUT", "0"}, {"REQUEST_TIMEOUT", "-3s"},
		{"SILENCE_THRESHOLD_DB", "NaN"}, {"SILENCE_THRESHOLD_DB", "+Inf"},
		{"SILENCE_THRESHOLD_DB", "-Inf"}, {"SILENCE_THRESHOLD_DB", "0.1"},
		{"SILENCE_THRESHOLD_DB", "invalid"},
		{"OBS_AUDIO_INPUTS", "Mic"}, {"OBS_AUDIO_INPUTS", "null"},
		{"OBS_AUDIO_INPUTS", `["Mic", 42]`}, {"OBS_AUDIO_INPUTS", `[""]`},
		{"OBS_AUDIO_INPUTS", `[" "]`}, {"OBS_AUDIO_INPUTS", `["Mic", "Mic"]`},
	} {
		t.Run(tc.key+"/"+tc.value, func(t *testing.T) {
			cleanEnvironment(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error = %v, want error naming %s", err, tc.key)
			}
		})
	}
}

func TestValidURLs(t *testing.T) {
	for _, value := range []string{
		"ws://127.0.0.1:4455", "wss://example.com", "ws://localhost:1/obs",
		"wss://example.com:65535/path?key=value", "ws://[::1]:4455", "ws://[fe80::1%25en0]:4455",
	} {
		if err := validateURL(value); err != nil {
			t.Errorf("%s: %v", value, err)
		}
	}
}

func TestInvalidURLs(t *testing.T) {
	for _, value := range []string{
		"", "http://localhost:4455", "localhost:4455", "ws://", "ws:///path",
		"ws://:4455", "ws://localhost:", "ws://localhost:0", "ws://localhost:65536",
		"ws://localhost:abc", "ws://user:secret@localhost:4455", "ws://localhost:4455#fragment",
		"ws://localhost:4455#", "ws://localhost]:4455", "ws://[localhost]:4455", "ws://::1:4455", "ws://[127.0.0.1]:4455",
	} {
		if err := validateURL(value); err == nil {
			t.Errorf("accepted invalid URL %q", value)
		}
	}
}

func TestErrorsDoNotExposeSecrets(t *testing.T) {
	const secret = "a-secret-that-must-not-appear"
	for _, tc := range []struct{ key, value string }{
		{"OBS_WS_URL", "ws://name:" + secret + "@host"},
		{"SILENCE_DURATION", secret}, {"SILENCE_THRESHOLD_DB", secret},
		{"OBS_AUDIO_INPUTS", secret},
	} {
		t.Run(tc.key, func(t *testing.T) {
			cleanEnvironment(t)
			t.Setenv(tc.key, tc.value)
			_, err := Load("")
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Errorf("unsafe or missing error: %v", err)
			}
		})
	}
	t.Run("malformed file", func(t *testing.T) {
		cleanEnvironment(t)
		_, err := Load(writeEnv(t, "OBS_WS_PASSWORD='"+secret+"\n"))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("unsafe or missing error: %v", err)
		}
	})
}

func TestPositiveSubsecondDurationsAndExactInputNames(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv("SILENCE_DURATION", "250ms")
	t.Setenv("SILENCE_THRESHOLD_DB", "0")
	t.Setenv("OBS_AUDIO_INPUTS", `[" Mic ", "mic"]`)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SilenceDuration != 250*time.Millisecond || cfg.ThresholdDB != 0 {
		t.Fatalf("duration or threshold not preserved: %#v", cfg)
	}
	if !reflect.DeepEqual(cfg.Inputs, []string{" Mic ", "mic"}) {
		t.Errorf("input names changed: %#v", cfg.Inputs)
	}
}
