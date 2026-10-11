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
	SchemaVersion int                  `json:"schema_version"`
	Tasks         []taskcenter.Summary `json:"tasks"`
}

func execute(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("cloudfile-taskcenter", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	list := flags.Bool("list", false, "print task summaries and exit")
	watch := flags.Bool("watch", false, "watch local stable saves, without uploading")
	workspaceRoot := flags.String("workspace-root", "", "optional local diagnostic override")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*list == *watch) {
		return errors.New("choose exactly one of --list or --watch")
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
	if *list {
		tasks, err := store.List()
		if err != nil {
			return err
		}
		return enc.Encode(listResult{SchemaVersion: 1, Tasks: tasks})
	}
	monitor, err := taskcenter.NewMonitor(store, 3*time.Second)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// One initial scan records pre-existing local changes without immediately
	// announcing them as new saves. Later polls are advisory only.
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
