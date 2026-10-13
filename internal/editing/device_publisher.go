package editing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// DeviceHTTPPublisher implements the device-only upload wire contract. The
// server must mount its native-guarded upload and commit-status routes
// before a controller may advertise this capability. A 404/503 never falls
// back to browser authentication or ordinary Seafile upload APIs.
var uploadTicket = regexp.MustCompile(`^v1\.[A-Za-z0-9_-]+$`)

type DeviceHTTPPublisher struct{ broker *DeviceCommitBroker }

func NewDeviceHTTPPublisher(broker *DeviceCommitBroker) (*DeviceHTTPPublisher, error) {
	if broker == nil {
		return nil, errors.New("confirmed device broker required")
	}
	return &DeviceHTTPPublisher{broker: broker}, nil
}

func (p *DeviceHTTPPublisher) signed(ctx context.Context, route string, body map[string]any) error {
	var response struct {
		Challenge DeviceCommitChallenge `json:"challenge"`
	}
	if err := p.broker.request(ctx, route, body, &response); err != nil {
		return err
	}
	c := response.Challenge
	if err := c.Validate(p.broker.instance, p.broker.deviceID, p.broker.sessionID, time.Now()); err != nil {
		return err
	}
	signature, err := p.broker.sign(ctx, c)
	if err != nil {
		return errors.New("device publication signing failed")
	}
	body["challenge"] = c
	body["signature"] = signature
	return nil
}

func (p *DeviceHTTPPublisher) PublishDeviceCommit(ctx context.Context, lease Lease, intent Intent) (Receipt, error) {
	// Small files use the already verified full-file publisher. For larger
	// files, a signed CDC missing-block plan decides between the two methods.
	if cdcEnabled() && intent.SnapshotSize >= 16<<20 {
		return p.publishDeviceCDC(ctx, lease, intent)
	}
	return p.publishDeviceFull(ctx, lease, intent)
}

func (p *DeviceHTTPPublisher) publishDeviceFull(ctx context.Context, lease Lease, intent Intent) (Receipt, error) {
	body, err := p.broker.commitRequest(lease, intent)
	if err != nil {
		return Receipt{}, err
	}
	input, size, err := verifiedSnapshot(intent)
	if err != nil {
		return Receipt{}, err
	}
	defer input.Close()
	if size != intent.SnapshotSize {
		return Receipt{}, errors.New("snapshot size changed")
	}
	// Freeze the exact outgoing bytes BEFORE requesting a short-lived proof.
	// The raw upload carries no local filename or path.
	staged, err := os.CreateTemp(filepath.Dir(intent.Snapshot), ".device-upload-*")
	if err != nil {
		return Receipt{}, err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	digest := sha256.New()
	count, err := io.Copy(io.MultiWriter(staged, digest), io.LimitReader(input, maxSnapshotBytes+1))
	if err != nil || count != size || hex.EncodeToString(digest.Sum(nil)) != intent.Digest {
		return Receipt{}, errors.New("snapshot changed while preparing upload")
	}
	if err := staged.Sync(); err != nil {
		return Receipt{}, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return Receipt{}, err
	}
	info, err := staged.Stat()
	if err != nil {
		return Receipt{}, err
	}
	// A new challenge is required: commit-authorize already consumed Prepare's
	// nonce, and a replay must never become authority to send file bytes.
	if err := p.signed(ctx, "commit-challenge", body); err != nil {
		return Receipt{}, err
	}
	var grant SharedUploadGrant
	if err := p.broker.request(ctx, "commit-upload-authorize", body, &grant); err != nil {
		return Receipt{}, err
	}
	const endpoint = "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/"
	data, err := UploadFrozenFull(ctx, p.broker, grant, intent, endpoint, staged, info.Size())
	if err != nil {
		return Receipt{}, err
	}
	return DecodeFullReceipt(data, intent)
}

func (p *DeviceHTTPPublisher) QueryDeviceCommit(ctx context.Context, intent Intent) (Receipt, error) {
	if !canonicalUUID.MatchString(intent.ID) {
		return Receipt{}, errors.New("canonical intent required")
	}
	body := map[string]any{"session_id": p.broker.sessionID, "device_id": p.broker.deviceID, "intent_id": intent.ID}
	if err := p.signed(ctx, "commit-status-challenge", body); err != nil {
		return Receipt{}, err
	}
	var result struct {
		Intent Receipt `json:"intent"`
	}
	if err := p.broker.request(ctx, "commit-status", body, &result); err != nil {
		return Receipt{}, err
	}
	if err := validateDeviceReceipt(result.Intent, intent, false); err != nil {
		return Receipt{}, err
	}
	return result.Intent, nil
}

func validateDeviceReceipt(receipt Receipt, intent Intent, published bool) error {
	if receipt.IntentID != intent.ID {
		return errors.New("receipt belongs to another intent")
	}
	if !published && (receipt.State == "not-found" || receipt.State == "prepared") {
		if receipt.CheckedIn || receipt.FileID != "" {
			return errors.New("unpublished receipt claims success")
		}
		return nil
	}
	if receipt.State != "published" || !objectID.MatchString(receipt.FileID) || receipt.ContentDigest != intent.Digest || receipt.ExpectedFileID != intent.ExpectedFileID || receipt.Action != intent.Action || receipt.CheckedIn != (intent.Action == "checkin") {
		return errors.New("native published receipt does not match manual commit")
	}
	return nil
}

var _ DeviceCommitPublisher = (*DeviceHTTPPublisher)(nil)
