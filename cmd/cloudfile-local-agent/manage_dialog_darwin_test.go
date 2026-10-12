//go:build darwin && cgo

package main

import (
	"testing"
)

func TestOriginalMacEditingWindowPerSessionExclusion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	session := "11111111-1111-4111-8111-111111111111"
	release, err := acquireMacSessionDialog(session)
	if err != nil {
		t.Fatal(err)
	}
	if competing, err := acquireMacSessionDialog(session); err == nil {
		competing()
		release()
		t.Fatal("second parallel manual dialog accepted")
	}
	release()
	again, err := acquireMacSessionDialog(session)
	if err != nil {
		t.Fatalf("dialog did not release original lock %v", err)
	}
	again()
	if _, err := acquireMacSessionDialog("../arbitrary"); err == nil {
		t.Fatal("path traversal in session dialog name")
	}
}
