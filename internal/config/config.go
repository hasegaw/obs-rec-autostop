// Package config loads configuration without changing the process environment.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config contains the OBS connection and audio silence detection settings.
type Config struct {
	URL             string
	Password        string
	SilenceDuration time.Duration
	ThresholdDB     float64
	Inputs          []string
	MeterTimeout    time.Duration
	RequestTimeout  time.Duration
}

// Load reads envPath, then overlays existing environment variables. An empty
// envPath disables file loading. A missing default .env file is optional, while
// any other explicitly selected file must exist. Neither loading nor validation
// modifies the process environment or includes configuration values in errors.
func Load(envPath string) (Config, error) {
	values := make(map[string]string)
	if envPath != "" {
		var err error
		values, err = godotenv.Read(envPath)
		if err != nil {
			if envPath != ".env" || !errors.Is(err, os.ErrNotExist) {
				return Config{}, errors.New("cannot read or parse environment file")
			}
			values = make(map[string]string)
		}
	}
	get := func(key, fallback string) string {
		if value, ok := os.LookupEnv(key); ok {
			return value
		}
		if value, ok := values[key]; ok {
			return value
		}
		return fallback
	}

	cfg := Config{
		URL:      get("OBS_WS_URL", "ws://127.0.0.1:4455"),
		Password: get("OBS_WS_PASSWORD", ""),
	}
	if err := validateURL(cfg.URL); err != nil {
		return Config{}, err
	}
	var err error
	if cfg.SilenceDuration, err = positiveDuration("SILENCE_DURATION", get("SILENCE_DURATION", "10m")); err != nil {
		return Config{}, err
	}
	if cfg.MeterTimeout, err = positiveDuration("METER_TIMEOUT", get("METER_TIMEOUT", "5s")); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = positiveDuration("REQUEST_TIMEOUT", get("REQUEST_TIMEOUT", "10s")); err != nil {
		return Config{}, err
	}
	cfg.ThresholdDB, err = strconv.ParseFloat(get("SILENCE_THRESHOLD_DB", "-50"), 64)
	if err != nil || math.IsNaN(cfg.ThresholdDB) || math.IsInf(cfg.ThresholdDB, 0) || cfg.ThresholdDB > 0 {
		return Config{}, errors.New("SILENCE_THRESHOLD_DB must be a finite number less than or equal to 0")
	}
	if err := json.Unmarshal([]byte(get("OBS_AUDIO_INPUTS", "[]")), &cfg.Inputs); err != nil || cfg.Inputs == nil {
		return Config{}, errors.New("OBS_AUDIO_INPUTS must be a JSON array of input names")
	}
	seen := make(map[string]bool, len(cfg.Inputs))
	for _, name := range cfg.Inputs {
		if strings.TrimSpace(name) == "" {
			return Config{}, errors.New("OBS_AUDIO_INPUTS must not contain empty input names")
		}
		if seen[name] {
			return Config{}, errors.New("OBS_AUDIO_INPUTS must not contain duplicate input names")
		}
		seen[name] = true
	}
	return cfg, nil
}

func positiveDuration(key, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 30s or 10m", key)
	}
	return duration, nil
}

func validateURL(value string) error {
	invalid := errors.New("OBS_WS_URL must be a ws:// or wss:// URL with a valid host and optional port, without credentials or a fragment")
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Opaque != "" || u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(value, "#") {
		return invalid
	}
	host := u.Hostname()
	if host == "" || strings.ContainsAny(host, " \t\r\n/\\?#@[]") || strings.HasSuffix(u.Host, ":") {
		return invalid
	}
	if strings.HasPrefix(u.Host, "[") {
		// net/url parses bracketed hosts without checking that they are IPv6.
		address := host
		if zone := strings.LastIndexByte(address, '%'); zone >= 0 {
			if zone == len(address)-1 {
				return invalid
			}
			address = address[:zone]
		}
		if net.ParseIP(address) == nil || !strings.Contains(address, ":") {
			return invalid
		}
	} else if strings.Contains(host, ":") {
		// IPv6 addresses in a URL must use brackets.
		return invalid
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return invalid
		}
	}
	return nil
}
