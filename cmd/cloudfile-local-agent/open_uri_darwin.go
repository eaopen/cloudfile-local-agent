//go:build darwin && cgo

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"github.com/eaopen/cloudfile-local-agent/internal/editing"
)

type pairedOpenURI struct{ Instance, SessionID, Ticket string }

var safeMacExtension = regexp.MustCompile(`^(?:\.[A-Za-z0-9]{1,16})?$`)

func parsePairedOpenURI(raw string) (pairedOpenURI, error) {
	fail := errors.New("invalid or unpaired CloudFile local URI")
	if len(raw) > 2048 || len(raw) < 80 || strings.TrimSpace(raw) != raw ||
		strings.IndexFunc(raw, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return pairedOpenURI{}, fail
	}
	const prefix = "cloudfile-open://v1/open?instance="
	if !strings.HasPrefix(raw, prefix) {
		return pairedOpenURI{}, fail
	}
	parts := strings.Split(strings.TrimPrefix(raw, prefix), "&")
	if len(parts) != 3 || !strings.HasPrefix(parts[1], "session=") ||
		!strings.HasPrefix(parts[2], "ticket=") {
		return pairedOpenURI{}, fail
	}
	instance, err := url.QueryUnescape(parts[0])
	sid := strings.TrimPrefix(parts[1], "session=")
	ticket := strings.TrimPrefix(parts[2], "ticket=")
	if err != nil || device.Origin(instance) != nil || !device.ID.MatchString(sid) ||
		!device.Encoded(ticket, 32) || raw != prefix+url.QueryEscape(instance)+"&session="+sid+"&ticket="+ticket {
		return pairedOpenURI{}, fail
	}
	return pairedOpenURI{instance, sid, ticket}, nil
}
func confirmedMacDevice(instance string) (string, error) {
	dir, err := pairingDirectory()
	if err != nil {
		return "", errors.New("paired Keychain unavailable")
	}
	sum := sha256.Sum256([]byte(instance))
	filename := filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > componentBudget ||
		info.Mode().Perm()&0077 != 0 {
		return "", errors.New("current-user paired device required")
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		return "", errors.New("paired device record unavailable")
	}
	var pair macPairing
	if strictComponentJSON(raw, &pair) != nil || pair.Instance != instance ||
		!device.ID.MatchString(pair.DeviceID) || pair.ExpiresAt != 0 ||
		!device.ID.MatchString(pair.Alias) || len(pair.Thumbprint) == 0 {
		return "", errors.New("confirmed Keychain pairing mismatch")
	}
	return pair.DeviceID, nil
}

func macDialog(prompt string, options ...string) (string, error) {
	// Static human-facing text and buttons only. Never insert a server path,
	// URI, ticket, cloud filename or shell argument into AppleScript.
	if prompt != "open" && prompt != "save" && prompt != "pending" &&
		prompt != "checkout" {
		return "", errors.New("invalid local decision")
	}
	var script string
	switch prompt {
	case "open":
		script = `button returned of (display dialog "CloudFile: open one file from the currently paired server?" buttons {"Cancel", "Open Local File"} default button "Open Local File" with icon note)`
	case "save":
		script = `button returned of (display dialog "Save in your local application first. Ctrl+S never uploads. Choose explicit CloudFile action." buttons {"Keep Local and Close", "Save Cloud and Continue", "Save Cloud and Finish"} default button "Save Cloud and Continue" with icon note)`
	case "checkout":
		script = `set chosen to choose from list {"Keep Local and Close", "Commit Cloud and Continue", "Commit and Checkin", "Abandon Checkout"} with prompt "CloudFile Checkout renewal runs while this dialog is open. Save locally first, then choose an explicit action." with title "CloudFile Exclusive Edit"
if chosen is false then
    return "Keep Local and Close"
end if
return item 1 of chosen`
	case "pending":
		script = `button returned of (display dialog "Original cloud upload is not yet confirmed. Do not create another upload. Check its original signed Intent?" buttons {"Keep Local and Close", "Recover Original Upload"} default button "Recover Original Upload" with icon caution)`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Hour)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script).Output()
	if err != nil {
		return "", errors.New("local user cancelled or no macOS dialog")
	}
	choice := strings.TrimSpace(string(output))
	for _, allowed := range options {
		if choice == allowed {
			return choice, nil
		}
	}
	return "", errors.New("unconfirmed local user choice")
}
func pairedMacProof(ctx context.Context, broker *editing.DeviceCommitBroker, sign editing.CommitChallengeSigner,
	sid, did, ticket, action string) (map[string]any, error) {
	// Server-generated challenge, always signed using the confirmed Keychain
	// pairing. No URI secret may act as a bare file or write capability.
	req := map[string]any{"session_id": sid, "device_id": did}
	if action == "claim" || action == "read" {
		req["ticket"] = ticket
	}
	var first struct {
		Challenge editing.DeviceCommitChallenge `json:"challenge"`
	}
	stage := "read-challenge"
	if action == "claim" {
		stage = "challenge"
	}
	if err := broker.RequestPairedSession(ctx, stage, req, &first); err != nil {
		return nil, err
	}
	if err := first.Challenge.ValidateSessionProof(broker.Instance(), did, sid, action, time.Now()); err != nil {
		return nil, err
	}
	signature, err := sign(ctx, first.Challenge)
	if err != nil {
		return nil, errors.New("paired Keychain proof failed")
	}
	body := map[string]any{"session_id": sid, "device_id": did,
		"challenge": first.Challenge, "signature": signature}
	if action == "claim" {
		body["ticket"] = ticket
	}
	return body, nil
}

type macReadGrant struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
	Transfer  struct {
		Path          string `json:"path"`
		Method        string `json:"method"`
		Authorization string `json:"authorization"`
		Redirects     bool   `json:"redirects"`
		Resume        bool   `json:"resume"`
	} `json:"transfer"`
	File struct {
		Extension     string `json:"extension"`
		LocalOpenType string `json:"local_open_type"`
		Mode          string `json:"mode"`
		BaseVersion   string `json:"base_version"`
	} `json:"file"`
}

func macGuardedDownload(ctx context.Context, instance, sid string, grant macReadGrant) (string, error) {
	if !device.ID.MatchString(grant.Ticket) || grant.ExpiresIn != 60 ||
		grant.Transfer.Path != "/seafhttp/cloudfile/read" ||
		grant.Transfer.Method != "GET" || grant.Transfer.Authorization != "Bearer" ||
		grant.Transfer.Redirects || grant.Transfer.Resume || !safeMacExtension.MatchString(grant.File.Extension) ||
		!regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(grant.File.BaseVersion) {
		return "", errors.New("guarded file read grant invalid")
	}
	ext := grant.File.Extension
	if ext == "" {
		ext = ".bin"
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "CloudFileAgent2", "local-edit", sid)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 || dirInfo.Mode().Perm()&0077 != 0 {
		return "", errors.New("private local edit workspace required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, instance+grant.Transfer.Path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+grant.Ticket)
	req.Header.Set("Accept-Encoding", "identity")
	client := http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return "", errors.New("guarded native read unconfirmed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.ContentLength < 0 || res.ContentLength > 128<<20 ||
		res.Header.Get("Content-Encoding") != "" || res.Header.Get("Location") != "" ||
		res.Header.Get("Content-Range") != "" {
		return "", errors.New("bounded complete guarded file required")
	}
	f, err := os.CreateTemp(dir, ".cloudfile-download-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	completed := false
	defer func() {
		f.Close()
		if !completed {
			os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0600); err != nil {
		return "", err
	}
	read, err := io.Copy(f, io.LimitReader(res.Body, res.ContentLength+1))
	if err != nil || read != res.ContentLength {
		return "", errors.New("incomplete guarded file copy")
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, hex.EncodeToString(random[:])+ext)
	if err = os.Rename(tmp, dest); err != nil {
		return "", err
	}
	completed = true
	return dest, nil
}
func runOpenURIFromStdin() error {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 2049))
	if err != nil || len(raw) > 2048 {
		return errors.New("bounded paired URI required")
	}
	open, err := parsePairedOpenURI(string(raw))
	if err != nil {
		return err
	}
	did, err := confirmedMacDevice(open.Instance)
	if err != nil {
		return err
	}
	choice, err := macDialog("open", "Open Local File")
	if err != nil || choice != "Open Local File" {
		return errors.New("file open not approved")
	}
	sign, err := installedEditSigner(open.Instance, did, open.SessionID)
	if err != nil {
		return err
	}
	broker, err := editing.NewDeviceCommitBroker(&http.Client{}, open.Instance, open.SessionID, did, sign)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	claim, err := pairedMacProof(ctx, broker, sign, open.SessionID, did, open.Ticket, "claim")
	if err != nil {
		return err
	}
	var claimReceipt struct {
		SessionID string `json:"session_id"`
		State     string `json:"state"`
	}
	if err = broker.RequestPairedSession(ctx, "claim", claim, &claimReceipt); err != nil {
		return errors.New("one-use device claim unconfirmed; do not retry ticket")
	}
	if claimReceipt.SessionID != open.SessionID || claimReceipt.State != "claimed" {
		return errors.New("paired session claim rejected")
	}
	proof, err := pairedMacProof(ctx, broker, sign, open.SessionID, did, open.Ticket, "read")
	if err != nil {
		return err
	}
	var grant macReadGrant
	if err = broker.RequestPairedSession(ctx, "read-ticket", proof, &grant); err != nil {
		return err
	}
	if grant.File.Mode != "optimistic-edit" && grant.File.Mode != "exclusive-edit" {
		return errors.New("unsupported guarded local edit mode")
	}
	if grant.File.Mode == "exclusive-edit" {
		init := checkoutInitRequest{Instance: open.Instance, DeviceID: did,
			SessionID: open.SessionID, UserConfirmed: true}
		data, _ := json.Marshal(init)
		if err := runCheckoutInit(bytes.NewReader(data), io.Discard); err != nil {
			return errors.New("current device Checkout initialization unavailable; retain server lock")
		}
	}
	local, err := macGuardedDownload(ctx, open.Instance, open.SessionID, grant)
	if err != nil {
		return err
	}
	if err = exec.Command("/usr/bin/open", local).Start(); err != nil {
		return errors.New("local application unavailable; file kept")
	}
	// v0.8: only this path owns confirmed Keychain pairing + signed claim
	// and signed read grant. The RC protocol has no repo/path: persist a
	// session-scoped task without inventing a library identity. Errors in the
	// advisory task index must never cancel the already-started v0.7 editor.
	if err := registerPairedMacTask(open.Instance, did, open.SessionID,
		grant.File.Mode, grant.File.BaseVersion, local); err != nil {
		fmt.Fprintln(os.Stderr, "CloudFile task display unavailable; the local edit remains open")
	}
	if grant.File.Mode == "exclusive-edit" {
		return macCheckoutController(open.Instance, did, open.SessionID, local)
	}
	base := grant.File.BaseVersion
	for {
		action, dialogErr := macDialog("save", "Keep Local and Close", "Save Cloud and Continue", "Save Cloud and Finish")
		if dialogErr != nil || action == "Keep Local and Close" {
			return nil
		}
		request := optimisticUploadRequest{Instance: open.Instance, SessionID: open.SessionID,
			DeviceID: did, BaseFileID: base, WorkingCopy: local, Action: "upload", UserConfirmed: true}
		encoded, _ := json.Marshal(request)
		var response bytes.Buffer
		uploadErr := runOptimisticUpload(bytes.NewReader(encoded), &response)
		var received struct {
			State  string                   `json:"state"`
			Result editing.OptimisticResult `json:"result"`
		}
		if uploadErr == nil && json.Unmarshal(response.Bytes(), &received) == nil &&
			received.State == "published" && received.Result.State == "published" &&
			received.Result.BaseFileID == base && len(received.Result.FileID) == 40 {
			base = received.Result.FileID
			if action == "Save Cloud and Finish" {
				return nil
			}
			continue
		}
		// Any unknown outcome is reconciled with the SAME saved original Intent.
		// User cannot accidentally start a second upload during this loop.
		for {
			decide, err := macDialog("pending", "Keep Local and Close", "Recover Original Upload")
			if err != nil || decide == "Keep Local and Close" {
				return nil
			}
			request.Action = "recover"
			request.WorkingCopy = ""
			encoded, _ = json.Marshal(request)
			response.Reset()
			recoverErr := runOptimisticUpload(bytes.NewReader(encoded), &response)
			if recoverErr != nil {
				continue
			}
			if json.Unmarshal(response.Bytes(), &received) == nil &&
				received.State == "published" && received.Result.State == "published" &&
				received.Result.BaseFileID == base && len(received.Result.FileID) == 40 {
				base = received.Result.FileID
				break
			}
		}
		if action == "Save Cloud and Finish" {
			return nil
		}
	}
}

// macCheckoutController is deliberately v0.7 single-file, foreground and
// explicit. It reuses the actual Checkout Journal and Go Upload Core, not a
// second browser credential or a background v0.8 TaskManager.
func macCheckoutController(instance, did, sid, local string) error {
	renew := func() bool {
		req, _ := json.Marshal(checkoutInitRequest{Instance: instance, DeviceID: did,
			SessionID: sid, UserConfirmed: true})
		var output bytes.Buffer
		if err := runCheckoutRenew(bytes.NewReader(req), &output); err != nil {
			return false
		}
		var confirmation struct {
			Renewed bool `json:"renewed"`
		}
		return json.Unmarshal(output.Bytes(), &confirmation) == nil && confirmation.Renewed
	}
	var valid atomic.Bool
	valid.Store(renew())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTicker(4 * time.Minute)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				valid.Store(renew())
			}
		}
	}()
	defer func() { cancel(); <-done }()
	for {
		selection, err := macDialog("checkout", "Keep Local and Close",
			"Commit Cloud and Continue", "Commit and Checkin", "Abandon Checkout")
		if err != nil || selection == "Keep Local and Close" {
			// No implicit release on close. Expiry is server-governed.
			return nil
		}
		if !valid.Load() && !renew() {
			return errors.New("Checkout lease unconfirmed; local copy retained")
		}
		valid.Store(true)
		action := "commit"
		switch selection {
		case "Commit and Checkin":
			action = "checkin"
		case "Abandon Checkout":
			action = "abandon"
		}
		request := editUploadRequest{Instance: instance, DeviceID: did, SessionID: sid,
			Action: action, UserConfirmed: true}
		if action != "abandon" {
			request.WorkingCopy = local
		}
		data, _ := json.Marshal(request)
		var result bytes.Buffer
		submitErr := runEditUpload(bytes.NewReader(data), &result)
		if action == "abandon" {
			if submitErr != nil {
				return errors.New("Checkout release unknown; preserve local copy and Journal")
			}
			var release struct {
				State    string `json:"state"`
				Released bool   `json:"released"`
			}
			if json.Unmarshal(result.Bytes(), &release) != nil ||
				release.State != "abandoned" || !release.Released {
				return errors.New("Checkout release not confirmed")
			}
			return nil
		}
		var published struct {
			Intent   editing.Receipt `json:"intent"`
			State    string          `json:"state"`
			IntentID string          `json:"intent_id"`
		}
		_ = json.Unmarshal(result.Bytes(), &published)
		if submitErr == nil && published.Intent.State == "published" &&
			published.Intent.CheckedIn == (action == "checkin") {
			if action == "checkin" {
				return nil
			}
			continue
		}
		// A lost ACK must be reconciled with exactly its durable Intent.
		original := published.IntentID
		if !device.ID.MatchString(original) {
			return errors.New("unknown Checkout publish; preserve local Journal and original snapshot")
		}
		for {
			choice, dialogErr := macDialog("pending", "Keep Local and Close", "Recover Original Upload")
			if dialogErr != nil || choice == "Keep Local and Close" {
				return nil
			}
			recovery := editUploadRequest{Instance: instance, DeviceID: did, SessionID: sid,
				Action: "recover", IntentID: original, UserConfirmed: true}
			data, _ = json.Marshal(recovery)
			result.Reset()
			if err = runEditUpload(bytes.NewReader(data), &result); err != nil {
				continue
			}
			if json.Unmarshal(result.Bytes(), &published) == nil &&
				published.Intent.State == "published" &&
				published.Intent.IntentID == original {
				if published.Intent.CheckedIn {
					return nil
				}
				valid.Store(renew())
				break
			}
		}
	}
}
