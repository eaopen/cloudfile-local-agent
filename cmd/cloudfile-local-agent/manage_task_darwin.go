//go:build darwin && cgo

package main

import (
	"io"
)

// This flag is NOT accepted by the Chrome Native Messaging Host.
// It only resumes original, explicitly consented macOS editor dialogs
// for a registered same-user TaskCenter session. The broker still signs
// every cloud operation; no browser path, token or automatic upload.
func runManageLocalTask(input io.Reader) error {
	task, err := inspectLocalTask(input)
	if err != nil {
		return err
	}
	if task.Mode == "exclusive-edit" {
		return macCheckoutController(task.Instance, task.DeviceID,
			task.SessionID, task.WorkingCopy)
	}
	return macOptimisticController(task.Instance, task.DeviceID,
		task.SessionID, task.BaseFileID, task.WorkingCopy)
}
