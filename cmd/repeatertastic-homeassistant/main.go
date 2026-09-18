// Command repeatertastic-homeassistant is a RepeaterTastic plugin that publishes the site and the
// nodes it hears to Home Assistant over MQTT.
//
// RepeaterTastic starts it with RT_PLUGIN_* in the environment and restarts it if it exits. To run
// it elsewhere, attach it in RepeaterTastic and set RT_PLUGIN_ID, RT_PLUGIN_ADDR and
// RT_PLUGIN_TOKEN; it then reconnects by itself.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ScotMesh/RepeaterTastic/sdk"

	"github.com/ScotMesh/repeatertastic-homeassistant/internal/hass"
)

var version = "dev"

// manifest is sent when attached, so RepeaterTastic can show the settings form.
//
//go:embed plugin.yaml
var manifest string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("repeatertastic-homeassistant", version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := sdk.Options{Version: version}
	managed := os.Getenv("RT_PLUGIN_SOCKET") != ""
	if !managed {
		opts.ManifestYAML = manifest
		if os.Getenv("RT_PLUGIN_ID") == "" {
			opts.ID = "homeassistant"
		}
	}
	if managed {
		// RepeaterTastic restarts a managed plugin that exits.
		if err := session(ctx, opts); err != nil && !errors.Is(err, context.Canceled) {
			log.Fatal(err)
		}
		return
	}
	// Attached: keep trying. RepeaterTastic refuses the session until the settings are filled in,
	// and the connection comes and goes with restarts.
	wait := 2 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := session(ctx, opts)
		if ctx.Err() != nil {
			return
		}
		log.Printf("disconnected: %v", err)
		if time.Since(start) > time.Minute {
			wait = 2 * time.Second // it was working, so try again promptly
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < 60*time.Second {
			wait *= 2
		}
	}
}

func session(ctx context.Context, opts sdk.Options) error {
	c, err := sdk.Connect(ctx, opts)
	if err != nil {
		return err
	}
	defer c.Close()

	var set hass.Settings
	if err := c.Settings(&set); err != nil {
		return fmt.Errorf("reading the settings: %w", err)
	}
	link := hass.New(c, set, version)
	_ = c.Log("info", "repeatertastic-homeassistant %s starting", version)
	return link.Run(c.Context())
}
