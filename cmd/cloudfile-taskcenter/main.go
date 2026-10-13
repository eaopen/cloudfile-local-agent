// A local read-only task observer for v0.8 development. It is NOT the Chrome
// Native Messaging Host, does not start a web server, and cannot upload files.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

type listResult struct {
	SchemaVersion      int                  `json:"schema_version"`
	Tasks              []taskcenter.Summary `json:"tasks"`
	ActiveExclusive    int                  `json:"active_exclusive"`
	UncertainExclusive int                  `json:"uncertain_exclusive"`
}

func execute(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("cloudfile-taskcenter", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	list := flags.Bool("list", false, "print task summaries and exit")
	watch := flags.Bool("watch", false, "resident local monitor; optional signed Checkout renewal")
	pollFocus := flags.Bool("poll-focus", false, "current-user GUI heartbeat and pending focus")
	goCore := flags.String("go-core-bin", "", "installed original Go Agent core for signed Checkout renewal")
	getPreferences := flags.Bool("get-preferences", false, "read local task UI preferences")
	setPosition := flags.Bool("set-position", false, "save a local floating-icon position")
	positionX := flags.Int("x", 0, "desktop X coordinate for --set-position")
	positionY := flags.Int("y", 0, "desktop Y coordinate for --set-position")
	display := flags.String("display", "", "local display identity for --set-position")
	workspaceRoot := flags.String("workspace-root", "", "optional local diagnostic override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	selected := 0
	for _, chosen := range []bool{*list, *watch, *getPreferences, *setPosition, *pollFocus} {
		if chosen {
			selected++
		}
	}
	if flags.NArg() != 0 || selected != 1 {
		return errors.New("choose exactly one taskcenter operation")
	}
	seen := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if *setPosition {
		if !seen["x"] || !seen["y"] {
			return errors.New("position requires explicit x and y coordinates")
		}
	} else if seen["x"] || seen["y"] || seen["display"] {
		return errors.New("position parameters require --set-position")
	}
	if *goCore != "" && !*watch {
		return errors.New("Go core is only used by the resident watcher")
	}
	root := *workspaceRoot
	if root == "" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		root, err = cfg.Root()
		if err != nil {
			return err
		}
	}
	store, err := taskcenter.New(root)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(true)
	if *getPreferences {
		prefs, err := store.Preferences()
		if err != nil {
			return err
		}
		return enc.Encode(prefs)
	}
	if *setPosition {
		prefs, err := store.Preferences()
		if err != nil {
			return err
		}
		prefs.FloatPosition = &taskcenter.FloatPosition{Display: *display, X: *positionX, Y: *positionY}
		if err := store.SavePreferences(prefs); err != nil {
			return err
		}
		return enc.Encode(map[string]bool{"ok": true})
	}
	if *pollFocus {
		taskID, err := store.PollFocus(time.Now())
		if err != nil {
			return err
		}
		return enc.Encode(map[string]string{"task_id": taskID})
	}
	if *list {
		tasks, err := store.List()
		if err != nil {
			return err
		}
		active, uncertain, err := exclusiveCounts(store)
		if err != nil {
			return err
		}
		return enc.Encode(listResult{SchemaVersion: 1, Tasks: tasks,
			ActiveExclusive: active, UncertainExclusive: uncertain})
	}
	keeper, err := newRenewal(store, *goCore)
	if err != nil {
		return err
	}
	monitor, err := taskcenter.NewMonitor(store, 3*time.Second)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// The local monitor stays resident even after a browser/editor dialog closes.
	// Signed renewal checks are at most once every 4 minutes per task.
	// Zero Core config produces a visible "unmanaged" warning, never fake renewal.
	nextRenew := time.Time{}
	poll := func() error {
		events, err := monitor.Scan(time.Now())
		if err != nil {
			return err
		}
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		if !time.Now().Before(nextRenew) {
			leaseEvents, err := keeper.RunOneCycle(ctx)
			if err != nil {
				if err := enc.Encode(leaseEvent{Kind: "checkout-needs-review"}); err != nil {
					return err
				}
			} else {
				for _, e := range leaseEvents {
					if err := enc.Encode(e); err != nil {
						return err
					}
				}
			}
			nextRenew = time.Now().Add(leaseInterval)
		}
		return nil
	}
	if err := poll(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := poll(); err != nil {
				return err
			}
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := execute(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "CloudFile local task inspection failed:", err)
		os.Exit(1)
	}
}
