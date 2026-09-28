package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/hasegaw/obs_rec_autostop/internal/config"
)

func main() {
	if err := runCLI(); err != nil {
		log.Printf("Error: %v", err)
		os.Exit(1)
	}
}

func runCLI() error {
	envPath := flag.String("env", ".env", ".env file path (empty: use environment only)")
	duration := flag.String("silence-duration", "", "override silence duration, e.g. 30s, 10m")
	dryRun := flag.Bool("dry-run", false, "exit when silence is detected without stopping recording")
	checkLevels := flag.Duration("check-levels", 0, "read and display audio levels for a duration without stopping recording, e.g. 10s")
	listInputs := flag.Bool("list-inputs", false, "test connection and list OBS input names")
	flag.Parse()
	if *checkLevels < 0 {
		return fmt.Errorf("-check-levels must be a positive duration")
	}
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments; use -help")
	}
	cfg, err := config.Load(*envPath)
	if err != nil {
		return err
	}
	if *duration != "" {
		parsed, err := time.ParseDuration(*duration)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("-silence-duration must be a positive duration, e.g. 30s or 10m")
		}
		cfg.SilenceDuration = parsed
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	logger := log.New(os.Stderr, "", log.LstdFlags)
	err = run(ctx, cfg, options{DryRun: *dryRun, ListInputs: *listInputs, CheckLevels: *checkLevels}, logger, os.Stdout)
	if ctx.Err() != nil {
		logger.Print("Monitoring stopped.")
		return nil
	}
	return err
}
