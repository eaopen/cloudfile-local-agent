package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/launch"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
	"github.com/eaopen/cloudfile-local-agent/internal/workspace"
)

// Called after confirmed pairing, signed read and successful local application
// launch. Do not expose this as an untrusted Chrome or public CLI registration
// API: callers cannot self-attest a signed CloudFile device session.
func registerPairedMacTask(instance, deviceID, sessionID, mode, baseFileID, copy string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	root, err := workspace.Resolve(cfg)
	if err != nil {
		return err
	}
	store, err := taskcenter.New(root)
	if err != nil {
		return err
	}
	if mode == "optimistic-edit" {
		mode = "local-edit"
	}
	_, _, err = store.RegisterVerifiedSession(taskcenter.VerifiedSession{
		Instance: instance, DeviceID: deviceID, SessionID: sessionID,
		Mode: mode, BaseFileID: baseFileID, WorkingCopy: copy,
	})
	if err == nil && !store.ResidentOnline(time.Now()) {
		// A previously opened portable .app can be discovered through the
		// current user's Launch Services (without admin or extra Host).
		// If it is not registered/allowed by Gatekeeper, editing stays usable.
		// -g explicitly avoids stealing focus while Office/CAD starts.
		// The complete v0.8 green package ships the .app next to the Go
		// controller, so no prior LaunchServices registration is required.
		// Never take any executable path from the remote session/Chrome.
		if executable, err := os.Executable(); err == nil {
			app := filepath.Join(filepath.Dir(executable), "CloudFileTaskCenter.app")
			if info, err := os.Lstat(app); err == nil &&
				info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				_ = launch.Detached("/usr/bin/open", "-g", app)
				return nil
			}
		}
		_ = launch.Detached("/usr/bin/open", "-g", "-b",
			"com.cloudfile.taskcenter.v08.preview")
	}
	return err
}
