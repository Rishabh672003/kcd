package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bethropolis/kcd/internal/events"
	"github.com/bethropolis/kcd/pkg/client"
	"github.com/urfave/cli/v2"
)

var watchCmd = &cli.Command{

	Name:  "watch",
	Usage: "Monitor real-time events from the daemon (notifications, battery, transfers)",
	Flags: []cli.Flag{
		&cli.StringSliceFlag{
			Name:    "events",
			Aliases: []string{"e"},
			Usage:   "Filter events by type (e.g. device.connected, battery.update)",
		},
		&cli.BoolFlag{
			Name:  "json",
			Usage: "Output raw NDJSON instead of human-readable text",
		},
	},
	Action: func(c *cli.Context) error {
		cl, err := getClient(c)
		if err != nil {
			return err
		}

		isJSON := c.Bool("json")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-stop
			cancel()
		}()

		ch := make(chan events.Event, 64)

		// Consumer goroutine
		go func() {
			for ev := range ch {
				if isJSON {
					b, _ := json.Marshal(ev)
					fmt.Println(string(b))
				} else {
					fmt.Print(formatEvent(ev))
				}
			}
		}()

		backoff := 1 * time.Second
		maxBackoff := 30 * time.Second

		for {
			if ctx.Err() != nil {
				return nil
			}

			start := time.Now()
			err := cl.Watch(ctx, c.StringSlice("events"), ch)

			// If err != nil, the connection failed or disconnected
			if err != nil {
				if err == context.Canceled {
					return nil
				}
				fmt.Fprintf(os.Stderr, "Daemon disconnected or not running: %v. Reconnecting in %v...\n", err, backoff)
			}

			if time.Since(start) > 5*time.Second {
				backoff = 1 * time.Second
			}

			select {
			case <-time.After(backoff):
				// Exponential backoff
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			case <-ctx.Done():
				return nil
			}
		}
	},
}

func watchDevices(c *cli.Context, cl *client.Client) error {
	devs, err := cl.Devices()
	if err != nil {
		return err
	}
	printDeviceTable(devs)

	ctx, cancel := context.WithCancel(c.Context)
	defer cancel()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		cancel()
	}()

	filters := []string{"device.connected", "device.disconnected", "device.added", "device.removed"}
	ch := make(chan events.Event, 8)
	go func() {
		for range ch {
			devs, err := cl.Devices()
			if err != nil {
				continue
			}
			fmt.Print("\033[2J\033[H") // clear screen
			printDeviceTable(devs)
		}
	}()

	return cl.Watch(ctx, filters, ch)
}
