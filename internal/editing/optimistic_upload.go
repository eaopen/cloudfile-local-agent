package editing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
)

// OptimisticUpload uses the existing paired-device signer but NEVER a
// Checkout lease, browser cookie or ordinary Seafile upload/update token.
// The server alone resolves the repository/path/actor from the claimed
// optimistic-edit session and checks the original File ID at native Branch.
type OptimisticUploader struct{ broker *DeviceCommitBroker }

func NewOptimisticUploader(b *DeviceCommitBroker) (*OptimisticUploader, error) {
	if b == nil {
		return nil, errors.New("paired device broker required")
	}
	return &OptimisticUploader{broker: b}, nil
}

type OptimisticResult struct {
	IntentID   string `json:"intent_id"`
	State      string `json:"state"`
	FileID     string `json:"result_file_id"`
	BaseFileID string `json:"base_file_id"`
}

var canonicalObjectID = regexp.MustCompile("^[a-f0-9]{40}$")

func (p *OptimisticUploader) Upload(ctx context.Context, intent Intent) (OptimisticResult, error) {
	if intent.Action != "optimistic-update" || !proofUUID.MatchString(intent.ID) ||
		!proofSHA.MatchString(intent.Digest) || !canonicalObjectID.MatchString(intent.ExpectedFileID) ||
		intent.SnapshotSize < 0 || intent.SnapshotSize > maxSnapshotBytes {
		return OptimisticResult{}, errors.New("original bounded optimistic edit required")
	}
	f, size, err := verifiedSnapshot(intent)
	if err != nil {
		return OptimisticResult{}, err
	}
	defer f.Close()
	if size != intent.SnapshotSize {
		return OptimisticResult{}, errors.New("captured optimistic snapshot size changed")
	}
	if cdcEnabled() && intent.SnapshotSize >= 16<<20 {
		publisher := DeviceHTTPPublisher{broker: p.broker}
		grantFn := func(ctx context.Context) (string, error) {
			grant, err := p.authorizedGrant(ctx, intent)
			return grant.Ticket, err
		}
		postFn := func(ctx context.Context, op, ticket string, item Intent, r io.Reader,
			length int64, kind, block string) ([]byte, error) {
			return publisher.cdcPostWithPrefix(ctx, "optimistic", op, ticket, item,
				r, length, kind, block)
		}
		data, fileID, used, err := executeSharedCDC(ctx, intent, grantFn, postFn)
		if err != nil {
			return OptimisticResult{}, err
		}
		if used {
			var response struct {
				Intent struct {
					IntentID      string `json:"intent_id"`
					State         string `json:"state"`
					BaseFileID    string `json:"base_file_id"`
					ResultFileID  string `json:"result_file_id"`
					ContentDigest string `json:"content_digest"`
				} `json:"intent"`
			}
			if json.Unmarshal(data, &response) != nil ||
				response.Intent.IntentID != intent.ID ||
				response.Intent.State != "published" ||
				response.Intent.ResultFileID != fileID ||
				response.Intent.BaseFileID != intent.ExpectedFileID ||
				response.Intent.ContentDigest != intent.Digest {
				return OptimisticResult{}, errors.New("native optimistic CDC receipt is unconfirmed")
			}
			return OptimisticResult{IntentID: intent.ID, State: "published",
				FileID: fileID, BaseFileID: intent.ExpectedFileID}, nil
		}
	}
	grant, err := p.authorizedGrant(ctx, intent)
	if err != nil {
		return OptimisticResult{}, err
	}
	const path = "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-upload/"
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return OptimisticResult{}, err
	}
	data, err := UploadFrozenFull(ctx, p.broker, grant, intent, path, f, size)
	if err != nil {
		return OptimisticResult{}, err
	}
	var result OptimisticResult
	if json.Unmarshal(data, &result) != nil || result.State != "published" ||
		result.IntentID != intent.ID || result.BaseFileID != intent.ExpectedFileID ||
		!canonicalObjectID.MatchString(result.FileID) {
		return OptimisticResult{}, errors.New("unbound native optimistic file receipt")
	}
	return result, nil
}

func (p *OptimisticUploader) authorizedGrant(ctx context.Context, intent Intent) (SharedUploadGrant, error) {
	body := map[string]any{"session_id": p.broker.sessionID,
		"device_id": p.broker.deviceID, "intent_id": intent.ID,
		"base_file_id": intent.ExpectedFileID, "content_digest": intent.Digest,
		"snapshot_size": intent.SnapshotSize}
	publisher := DeviceHTTPPublisher{broker: p.broker}
	if err := publisher.signed(ctx, "optimistic-upload-challenge", body); err != nil {
		return SharedUploadGrant{}, err
	}
	var grant SharedUploadGrant
	if err := p.broker.request(ctx, "optimistic-upload-authorize", body, &grant); err != nil {
		return SharedUploadGrant{}, err
	}
	const path = "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/optimistic-upload/"
	if err := ValidateUploadGrant(grant, intent, path); err != nil {
		return SharedUploadGrant{}, err
	}
	return grant, nil
}

// Query uses a fresh one-use paired-device proof to read the SAME durable
// optimistic Intent. It cannot upload or create a replacement Intent.
func (p *OptimisticUploader) Query(ctx context.Context, intent Intent) (OptimisticResult, error) {
	if !proofUUID.MatchString(intent.ID) || !canonicalObjectID.MatchString(intent.ExpectedFileID) ||
		!proofSHA.MatchString(intent.Digest) || intent.Action != "optimistic-update" {
		return OptimisticResult{}, errors.New("original optimistic recovery intent required")
	}
	body := map[string]any{"session_id": p.broker.sessionID,
		"device_id": p.broker.deviceID, "intent_id": intent.ID}
	publisher := DeviceHTTPPublisher{broker: p.broker}
	if err := publisher.signed(ctx, "optimistic-status-challenge", body); err != nil {
		return OptimisticResult{}, err
	}
	var result struct {
		Intent struct {
			IntentID      string `json:"intent_id"`
			State         string `json:"state"`
			BaseFileID    string `json:"base_file_id"`
			ContentDigest string `json:"content_digest"`
			SnapshotSize  int64  `json:"snapshot_size"`
			ResultFileID  string `json:"result_file_id"`
		} `json:"intent"`
	}
	if err := p.broker.request(ctx, "optimistic-status", body, &result); err != nil {
		return OptimisticResult{}, err
	}
	found := result.Intent
	if found.IntentID != intent.ID {
		return OptimisticResult{}, errors.New("cross-intent recovery status refused")
	}
	if found.State == "not-found" {
		return OptimisticResult{IntentID: intent.ID, State: "not-found"}, nil
	}
	if found.BaseFileID != intent.ExpectedFileID || found.ContentDigest != intent.Digest ||
		found.SnapshotSize != intent.SnapshotSize {
		return OptimisticResult{}, errors.New("original upload content changed on server")
	}
	if found.State == "published" && canonicalObjectID.MatchString(found.ResultFileID) {
		return OptimisticResult{IntentID: intent.ID, State: "published",
			FileID: found.ResultFileID, BaseFileID: intent.ExpectedFileID}, nil
	}
	if found.State == "prepared" && found.ResultFileID == "" {
		return OptimisticResult{IntentID: intent.ID, State: "prepared",
			BaseFileID: intent.ExpectedFileID}, nil
	}
	return OptimisticResult{}, errors.New("unconfirmed server publication state")
}

// The explicit opt-in is distinct from exclusive Checkout and CDC enablement.
// No optimistic upload is ever dispatched merely because the user saved a file.
func OptimisticUploadEnabled() bool {
	return os.Getenv("CLOUDFILE_AGENT2_OPTIMISTIC_UPLOAD_ENABLED") == "1"
}
