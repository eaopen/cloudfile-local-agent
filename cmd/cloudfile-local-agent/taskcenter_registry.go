package main

import (
	"github.com/eaopen/cloudfile-local-agent/internal/config"
	"github.com/eaopen/cloudfile-local-agent/internal/taskcenter"
)

// Called after confirmed pairing, signed read and successful local application
// launch. Do not expose this as an untrusted Chrome or public CLI registration
// API: callers cannot self-attest a signed CloudFile device session.
func registerPairedMacTask(instance, deviceID, sessionID, mode, baseFileID, copy string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	root, err := cfg.Root()
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
	return err
}
