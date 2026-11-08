// Command simulator plays N chargers against a running EV billing server.
//
// A replay of past times shows its sessions as "stale" on the status page
// until they are stopped (SES-4); that is expected.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/jlimty/ev-bill/internal/sim"
)

func main() {
	err := run(os.Args[1:], os.Getenv, os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return // usage was printed
	}
	if err != nil {
		slog.Error("simulator failed", "error", err)
		os.Exit(1)
	}
}

// run parses args and plays the chargers. Usage and flag errors go to out.
func run(args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("simulator", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		_, err := fmt.Fprintf(fs.Output(), "Usage: simulator [flags]\n\n"+
			"Plays N chargers against a running server. A replay of past times shows\n"+
			"its sessions as \"stale\" until they are stopped (SES-4); that is expected.\n\n")
		if err != nil {
			slog.Error("print usage", "error", err)
			return
		}
		fs.PrintDefaults()
	}
	var (
		baseURL  = fs.String("url", "http://localhost:8080", "server base URL")
		key      = fs.String("key", "", "API key (defaults to $API_KEY)")
		chargers = fs.Int("chargers", 3, "number of chargers, named CP-01, CP-02, ...")
		startStr = fs.String("start", "", "session start, RFC 3339 (default: now minus -duration)")
		duration = fs.Duration("duration", 155*time.Minute, "session length")
		interval = fs.Duration("interval", 5*time.Minute, "time between readings")
		powerW   = fs.Int64("power-w", 7200, "charging power in watts")
		speed    = fs.Int("speed", 60, "time compression; above 1 needs a session that has already ended")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The default is applied here, not in the flag, so usage never prints the key.
	if *key == "" {
		*key = getenv("API_KEY")
	}
	if *key == "" {
		return fmt.Errorf("no API key: set -key or $API_KEY")
	}
	if *chargers < 1 {
		return fmt.Errorf("-chargers must be at least 1, got %d", *chargers)
	}

	now := time.Now()
	start := now.Add(-*duration)
	if *startStr != "" {
		t, err := time.Parse(time.RFC3339, *startStr)
		if err != nil {
			return fmt.Errorf("-start: %w", err)
		}
		start = t
	}

	plans := make([]sim.Plan, *chargers)
	for i := range plans {
		plans[i] = sim.Plan{
			ChargerID: fmt.Sprintf("CP-%02d", i+1),
			Start:     start,
			Duration:  *duration,
			Interval:  *interval,
			PowerW:    *powerW,
		}
		if err := sim.Validate(plans[i], *speed, now); err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	slog.Info("simulator starting", "url", *baseURL, "chargers", *chargers,
		"start", start.UTC().Format(time.RFC3339), "duration", *duration,
		"interval", *interval, "power_w", *powerW, "speed", *speed)
	if err := sim.Run(ctx, sim.NewClient(*baseURL, *key), plans, *speed); err != nil {
		return err
	}
	slog.Info("simulator finished", "chargers", *chargers)
	return nil
}
