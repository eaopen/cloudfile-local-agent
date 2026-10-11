package editing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"
)

// SharedUploadGrant is transport-only: never confers publishing authority.
// Ordinary-edit and Checkout each obtain a signed, short-lived grant through
// their distinct authorization paths. The native server is authoritative.
type SharedUploadGrant struct {
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

var uploadEndpointPattern = regexp.MustCompile(`^/api/v2\.1/cloudfile/extensions/local-edit/v1/agent/(?:commit-upload|optimistic-upload)/$`)

// ValidateUploadGrant rejects attempts to switch one signed intent's bytes,
// operation or destination. Short-lived bearer scope is enforced server-side.
func ValidateUploadGrant(g SharedUploadGrant, intent Intent, path string) error {
	if !uploadEndpointPattern.MatchString(path) || !uploadTicket.MatchString(g.Ticket) ||
		len(g.Ticket) < 43 || len(g.Ticket) > 14000 ||
		g.ExpiresIn < 1 || g.ExpiresIn > 60 || g.IntentID != intent.ID ||
		g.Digest != intent.Digest || g.Size != intent.SnapshotSize ||
		!g.NativePublish || g.Transfer.Path != path || g.Transfer.Method != "POST" ||
		g.Transfer.Authorization != "Bearer" || g.Transfer.Redirects {
		return errors.New("signed upload grant does not match frozen original intent")
	}
	return nil
}

// UploadFrozenFull uses the same exact-byte POST protocol for all Agent edits.
// The caller supplies a revalidated snapshot reader and mode-specific grant.
// No Web cookie, arbitrary URL, application filename, or retry is permitted.
func UploadFrozenFull(ctx context.Context, broker *DeviceCommitBroker,
	g SharedUploadGrant, intent Intent, path string,
	snapshot io.Reader, size int64) ([]byte, error) {
	if broker == nil || size != intent.SnapshotSize || size < 0 || size > maxSnapshotBytes ||
		!canonicalUUID.MatchString(intent.ID) || !proofSHA.MatchString(intent.Digest) {
		return nil, errors.New("frozen single-file upload required")
	}
	if err := ValidateUploadGrant(g, intent, path); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, broker.instance+path, snapshot)
	if err != nil {
		return nil, errors.New("fixed device upload URL invalid")
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+g.Ticket)
	req.Header.Set("Idempotency-Key", intent.ID)
	req.Header.Set("Accept", "application/json")
	client := *broker.client
	client.Timeout = 5 * time.Minute
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("publication response unknown; inspect original intent")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 || response.StatusCode != http.StatusOK {
		return nil, errors.New("signed upload publication not confirmed; inspect original intent")
	}
	return data, nil
}

// PickCDC is purely the economics of transfer, never an authorization
// decision. A successful plan is not yet a publication; after stage/publish
// the original intent and method are pinned for recovery.
func PickCDC(total, missing int64) bool {
	if total < 16<<20 || missing < 0 || missing > total {
		return false
	}
	return total-missing >= 4<<20 && missing*4 <= total*3
}

// DecodeFullReceipt is a wire parsing helper, not proof of server authority.
func DecodeFullReceipt(data []byte, intent Intent) (Receipt, error) {
	var payload struct {
		Intent Receipt `json:"intent"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return Receipt{}, errors.New("invalid signed receipt JSON")
	}
	if err := validateDeviceReceipt(payload.Intent, intent, true); err != nil {
		return Receipt{}, err
	}
	return payload.Intent, nil
}
