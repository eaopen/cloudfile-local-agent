package editing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/blockdelta"
)

// Device CDC transport is explicitly enabled only for an already confirmed
// local manual Commit. It reuses the existing device-signed CommitIntent;
// staging a block never publishes a file or changes the active checkout.
func (p *DeviceHTTPPublisher) cdcGrant(ctx context.Context, lease Lease, intent Intent) (string, error) {
	body, err := p.broker.commitRequest(lease, intent)
	if err != nil {
		return "", err
	}
	if err = p.signed(ctx, "commit-challenge", body); err != nil {
		return "", err
	}
	var grant struct {
		Ticket        string `json:"ticket"`
		ExpiresIn     int    `json:"expires_in"`
		IntentID      string `json:"intent_id"`
		Digest        string `json:"content_digest"`
		Size          int64  `json:"snapshot_size"`
		NativePublish bool   `json:"native_device_publish"`
		Transfer      struct {
			Path          string `json:"path"`
			Method        string `json:"method"`
			Authorization string `json:"authorization"`
			Redirects     bool   `json:"redirects"`
		} `json:"transfer"`
	}
	if err = p.broker.request(ctx, "commit-upload-authorize", body, &grant); err != nil {
		return "", err
	}
	if len(grant.Ticket) < 43 || len(grant.Ticket) > 14000 ||
		!uploadTicket.MatchString(grant.Ticket) || grant.ExpiresIn < 1 || grant.ExpiresIn > 60 ||
		grant.IntentID != intent.ID || grant.Digest != intent.Digest ||
		grant.Size != intent.SnapshotSize || !grant.NativePublish ||
		grant.Transfer.Path != "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/" ||
		grant.Transfer.Method != "POST" || grant.Transfer.Authorization != "Bearer" ||
		grant.Transfer.Redirects {
		return "", errors.New("invalid native device grant for CDC")
	}
	return grant.Ticket, nil
}

func (p *DeviceHTTPPublisher) cdcPost(ctx context.Context, operation, grant string,
	intent Intent, body io.Reader, size int64, contentType, blockID string) ([]byte, error) {
	return p.cdcPostWithPrefix(ctx, "commit", operation, grant, intent,
		body, size, contentType, blockID)
}

func (p *DeviceHTTPPublisher) cdcPostWithPrefix(ctx context.Context, prefix, operation, grant string,
	intent Intent, body io.Reader, size int64, contentType, blockID string) ([]byte, error) {
	if prefix != "commit" && prefix != "optimistic" {
		return nil, errors.New("fixed device CDC authorization mode required")
	}
	if operation != "plan" && operation != "stage" && operation != "publish" {
		return nil, errors.New("fixed CDC operation required")
	}
	target := p.broker.instance + "/api/v2.1/cloudfile/extensions/local-edit/v1/agent/" + prefix + "-block-" + operation + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, body)
	if err != nil {
		return nil, errors.New("invalid fixed CDC target")
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+grant)
	req.Header.Set("Idempotency-Key", intent.ID)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if blockID != "" {
		req.Header.Set("X-CloudFile-Block-ID", blockID)
	}
	client := *p.broker.client
	client.Timeout = 3 * time.Minute
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("native block response unknown; recover original intent")
	}
	defer response.Body.Close()
	result, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(result) > 65536 || response.StatusCode != http.StatusOK {
		return nil, errors.New("signed CDC operation was not confirmed")
	}
	return result, nil
}

type deviceStagedCDC struct {
	grant    func(context.Context) (string, error)
	post     func(context.Context, string, string, Intent, io.Reader, int64, string, string) ([]byte, error)
	intent   Intent
	expected []string
}

func (s *deviceStagedCDC) Missing(_ context.Context, ordered []string) ([]string, error) {
	allowed := map[string]bool{}
	for _, id := range ordered {
		allowed[id] = true
	}
	seen := map[string]bool{}
	for _, id := range s.expected {
		if !allowed[id] || seen[id] {
			return nil, errors.New("invalid native missing blocks")
		}
		seen[id] = true
	}
	return append([]string(nil), s.expected...), nil
}
func (s *deviceStagedCDC) Put(ctx context.Context, id string, raw []byte) error {
	if len(raw) < 1 || len(raw) > blockdelta.MaxSize {
		return errors.New("invalid block byte budget")
	}
	grant, err := s.grant(ctx)
	if err != nil {
		return err
	}
	data, err := s.post(ctx, "stage", grant, s.intent,
		bytes.NewReader(raw), int64(len(raw)), "application/octet-stream", id)
	if err != nil {
		return err
	}
	var response struct {
		Staged   bool   `json:"staged"`
		BlockID  string `json:"block_id"`
		IntentID string `json:"intent_id"`
	}
	if json.Unmarshal(data, &response) != nil || !response.Staged ||
		response.BlockID != id || response.IntentID != s.intent.ID {
		return errors.New("unconfirmed staged CDC block")
	}
	return nil
}

func executeSharedCDC(ctx context.Context, intent Intent,
	grantFn func(context.Context) (string, error),
	postFn func(context.Context, string, string, Intent, io.Reader, int64, string, string) ([]byte, error),
) ([]byte, string, bool, error) {
	if intent.SnapshotSize == 0 || intent.SnapshotSize > maxSnapshotBytes {
		return nil, "", false, errors.New("CDC P0 limited to existing 128 MiB manual snapshots")
	}
	manifest, err := blockdelta.ScanSnapshot(intent.Snapshot, 1)
	if err != nil {
		return nil, "", false, err
	}
	if manifest.SHA256 != intent.Digest || manifest.Size != intent.SnapshotSize ||
		len(manifest.Blocks) == 0 || len(manifest.Blocks) > 64 {
		return nil, "", false, errors.New("immutable Seafile CDC manifest mismatch")
	}
	type manifestBlock struct {
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}
	blocks := make([]manifestBlock, 0, len(manifest.Blocks))
	for _, block := range manifest.Blocks {
		blocks = append(blocks, manifestBlock{block.ID, block.Size})
	}
	plan := struct {
		FileID string          `json:"file_id"`
		Size   int64           `json:"size"`
		SHA256 string          `json:"sha256"`
		Blocks []manifestBlock `json:"blocks"`
	}{manifest.FileID, manifest.Size, manifest.SHA256, blocks}
	payload, err := json.Marshal(plan)
	if err != nil || len(payload) > 65536 {
		return nil, "", false, errors.New("CDC plan exceeds bounded wire budget")
	}
	grant, err := grantFn(ctx)
	if err != nil {
		return nil, "", false, err
	}
	result, err := postFn(ctx, "plan", grant, intent,
		bytes.NewReader(payload), int64(len(payload)), "application/json", "")
	if err != nil {
		return nil, "", false, err
	}
	var planned struct {
		FileID       string   `json:"file_id"`
		IntentID     string   `json:"intent_id"`
		SnapshotSize int64    `json:"snapshot_size"`
		Missing      []string `json:"missing"`
	}
	if json.Unmarshal(result, &planned) != nil || planned.FileID != manifest.FileID ||
		planned.IntentID != intent.ID || planned.SnapshotSize != manifest.Size {
		return nil, "", false, errors.New("unconfirmed server CDC plan")
	}
	// Seafile's raw-block IDs are content-addressed; dedupe them when
	// estimating bytes, but retain original ordered blocks for File ID.
	// A first upload with few reusable blocks should use the proven full
	// transfer. This selection happens once BEFORE any raw block is staged,
	// retains the SAME prepared Intent, and never masks unknown publication.
	missing := make(map[string]bool, len(planned.Missing))
	var missingBytes int64
	byID := make(map[string]int64, len(manifest.Blocks))
	for _, block := range manifest.Blocks {
		byID[block.ID] = block.Size
	}
	for _, id := range planned.Missing {
		size, exists := byID[id]
		if !exists || missing[id] {
			return nil, "", false, errors.New("native missing-block response cannot be trusted")
		}
		missing[id] = true
		missingBytes += size
	}
	if !cdcWorthwhile(manifest.Size, missingBytes) {
		// A successful plan response is NOT a file publication, so switching
		// transport here is safe. The publisher still uses the same Intent.
		return nil, "", false, nil
	}
	stager := &deviceStagedCDC{grant: grantFn, post: postFn, intent: intent, expected: planned.Missing}
	staged, err := blockdelta.StageSnapshotOnly(ctx, intent.Snapshot, 1, stager)
	if err != nil || staged.Manifest.FileID != manifest.FileID ||
		staged.Manifest.SHA256 != manifest.SHA256 {
		return nil, "", false, errors.New("CDC stage incomplete; recover original Intent")
	}
	// Native publish is the ONLY write to the file path. It revalidates every
	// previously staged/reused block and Guard inside the Branch CAS.
	grant, err = grantFn(ctx)
	if err != nil {
		return nil, "", false, err
	}
	final, err := postFn(ctx, "publish", grant, intent, strings.NewReader(""), 0,
		"application/octet-stream", "")
	if err != nil {
		return nil, "", false, err
	}
	return final, manifest.FileID, true, nil
}

func (p *DeviceHTTPPublisher) publishDeviceCDC(ctx context.Context, lease Lease, intent Intent) (Receipt, error) {
	grantFn := func(ctx context.Context) (string, error) { return p.cdcGrant(ctx, lease, intent) }
	postFn := func(ctx context.Context, op, grant string, intent Intent, r io.Reader,
		size int64, typ, id string) ([]byte, error) {
		return p.cdcPost(ctx, op, grant, intent, r, size, typ, id)
	}
	data, fileID, used, err := executeSharedCDC(ctx, intent, grantFn, postFn)
	if err != nil {
		return Receipt{}, err
	}
	if !used {
		return p.publishDeviceFull(ctx, lease, intent)
	}
	var reply struct {
		Intent Receipt `json:"intent"`
	}
	if json.Unmarshal(data, &reply) != nil || reply.Intent.FileID != fileID {
		return Receipt{}, errors.New("native guarded CDC File ID unconfirmed")
	}
	if err = validateDeviceReceipt(reply.Intent, intent, true); err != nil {
		return Receipt{}, err
	}
	return reply.Intent, nil
}

func cdcEnabled() bool { return os.Getenv("CLOUDFILE_AGENT2_CDC_ENABLED") == "1" }

// Heuristic only: never overrides the server's final version/Guard fencing.
// Requiring at least 4 MiB saved and <=75% missing avoids paying CDC request
// and hashing cost for compressed or rewritten documents that reuse no blocks.
func cdcWorthwhile(total, missing int64) bool {
	return PickCDC(total, missing)
}
