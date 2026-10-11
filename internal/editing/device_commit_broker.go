package editing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CommitChallengeSigner signs only the actual server-issued current device
// commit challenge. On Windows it delegates to the already confirmed CNG key
// via the Agent1 1.2.1 .NET Bridge. macOS uses a protected Go P-256 key.
type CommitChallengeSigner func(context.Context, DeviceCommitChallenge) (string, error)

// DeviceCommitBroker prepares a real server CommitIntent after verifying a
// device's current one-time signature. This is NOT a publishing transport.
// An independent native guarded Publish implementation is still required,
// never the ordinary Seafile update or upload-blks commit API.
type DeviceCommitBroker struct {
	client              *http.Client
	base                string
	instance            string
	sessionID, deviceID string
	sign                CommitChallengeSigner
}

func NewDeviceCommitBroker(source *http.Client, instance, sessionID, deviceID string, signer CommitChallengeSigner) (*DeviceCommitBroker, error) {
	parsed, err := url.Parse(instance)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil ||
		strings.ToLower(parsed.Host) != parsed.Host || source == nil || signer == nil ||
		!proofUUID.MatchString(sessionID) || !proofUUID.MatchString(deviceID) {
		return nil, errors.New("paired HTTPS device and signer required")
	}
	copyClient := *source
	copyClient.Jar = nil
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copyClient.Timeout == 0 || copyClient.Timeout > 20*time.Second {
		copyClient.Timeout = 20 * time.Second
	}
	if trans, ok := copyClient.Transport.(*http.Transport); ok && trans.TLSClientConfig != nil && trans.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("verified TLS required for device authorization")
	}
	return &DeviceCommitBroker{client: &copyClient, instance: instance, base: instance + "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/",
		sessionID: sessionID, deviceID: deviceID, sign: signer}, nil
}

func (b *DeviceCommitBroker) request(ctx context.Context, operation string, request any, result any) error {
	data, err := json.Marshal(request)
	if err != nil || len(data) > 16384 {
		return errors.New("bounded device request required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+operation+"/", bytes.NewReader(data))
	if err != nil {
		return errors.New("device broker request invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return errors.New("device proof result unknown; inspect original commit intent")
	}
	defer resp.Body.Close()
	output, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if err != nil || len(output) > 16384 || resp.StatusCode != http.StatusOK {
		return errors.New("device broker did not confirm authorization")
	}
	if err := json.Unmarshal(output, result); err != nil {
		return errors.New("device broker response invalid")
	}
	return nil
}

// PrepareIntent performs a two-step, single-use challenge bound to the exact
// saved snapshot and guard. It never uploads a byte, and returns no bearer.
func (b *DeviceCommitBroker) PrepareIntent(ctx context.Context, lease Lease, intent Intent) error {
	if !proofUUID.MatchString(intent.ID) || !proofUUID.MatchString(lease.GuardID) ||
		!proofSHA.MatchString(intent.Digest) || !objectID.MatchString(lease.BaseFileID) ||
		lease.Generation == "" || lease.CredentialEpoch == "" ||
		len(lease.Token) != 64 || intent.ExpectedFileID != lease.BaseFileID ||
		intent.SnapshotSize < 0 || intent.SnapshotSize > maxSnapshotBytes ||
		(intent.Action != "commit" && intent.Action != "checkin") {
		return errors.New("guarded immutable manual commit required")
	}
	req := map[string]any{
		"session_id": b.sessionID, "device_id": b.deviceID,
		"intent_id": intent.ID, "guard_id": lease.GuardID,
		"generation": lease.Generation, "credential_epoch": lease.CredentialEpoch,
		"token": lease.Token, "base_file_id": intent.ExpectedFileID,
		"content_digest": intent.Digest, "snapshot_size": intent.SnapshotSize, "action": intent.Action,
	}
	var challenge struct {
		Challenge DeviceCommitChallenge `json:"challenge"`
	}
	if err := b.request(ctx, "commit-challenge", req, &challenge); err != nil {
		return err
	}
	if err := challenge.Challenge.Validate(b.instance, b.deviceID, b.sessionID, time.Now()); err != nil {
		return err
	}
	signature, err := b.sign(ctx, challenge.Challenge)
	if err != nil {
		return errors.New("paired device commit signing failed")
	}
	req["challenge"] = challenge.Challenge
	req["signature"] = signature
	var confirmed struct {
		IntentID         string `json:"intent_id"`
		State            string `json:"state"`
		DeviceAuthorized bool   `json:"device_authorized"`
		Publication      string `json:"publication"`
	}
	if err := b.request(ctx, "commit-authorize", req, &confirmed); err != nil {
		return err
	}
	if confirmed.IntentID != intent.ID || confirmed.State != "prepared" || !confirmed.DeviceAuthorized ||
		confirmed.Publication != "not_started" {
		return errors.New("device CommitIntent not confirmed")
	}
	return nil
}
