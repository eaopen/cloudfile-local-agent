package editing

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
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxSnapshotBytes = 128 * 1024 * 1024

var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Reference struct {
	RepoID string `json:"repo_id"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
}
type Guard struct {
	Lease
	Active        bool    `json:"active"`
	Mode          string  `json:"mode"`
	PendingIntent *string `json:"pending_intent"`
}

// NativeVersionReader must use a current authenticated server read. Values are
// concurrency conditions, never authority. A legacy local-open ticket cannot
// authenticate this transport or grant publication rights.
type NativeVersionReader func(context.Context) (headID, fileID string, err error)

// SessionHTTPTransport adapts the existing native OIDC HTTP contract. It is not
// installed in the v2 runner and accepts no cookies in a .cloudfile session.
// The caller must supply a trusted authenticated host client (including CSRF).
// A device-authentication broker must be verified before enabling Agent use.
type SessionHTTPTransport struct {
	client    *http.Client
	endpoint  string
	reference Reference
	version   NativeVersionReader
}

func NewSessionHTTPTransport(client *http.Client, endpoint string, ref Reference, version NativeVersionReader) (*SessionHTTPTransport, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		!canonicalUUID.MatchString(ref.RepoID) || ref.Kind != "file" || !strings.HasPrefix(ref.Path, "/") || strings.Contains(ref.Path, "\x00") ||
		strings.Contains(ref.Path, "//") || strings.Contains(ref.Path, "/../") || strings.HasSuffix(ref.Path, "/..") || strings.Contains(ref.Path, "/./") || len(ref.Path) > 4096 || client == nil || version == nil {
		return nil, errors.New("trusted HTTPS session, single-file target and native version reader required")
	}
	copyClient := *client
	if copyClient.Timeout <= 0 || copyClient.Timeout > 180*time.Second {
		copyClient.Timeout = 180 * time.Second
	}
	if transport, ok := copyClient.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("verified TLS is required for native editing credentials")
	}
	// The proof must never follow an HTTP redirect, even to the same host.
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &SessionHTTPTransport{client: &copyClient, endpoint: strings.TrimRight(endpoint, "/") + "/", reference: ref, version: version}, nil
}
func newIntentID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(data)
	return fmt.Sprintf("%s-%s-%s-%s-%s", raw[:8], raw[8:12], raw[12:16], raw[16:20], raw[20:]), nil
}
func (t *SessionHTTPTransport) send(ctx context.Context, operation, contentType, key string, body io.Reader, result any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+operation+"/", body)
	if err != nil {
		return err
	}
	if staged, ok := body.(*os.File); ok {
		info, err := staged.Stat()
		if err != nil {
			return err
		}
		request.ContentLength = info.Size()
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := t.client.Do(request)
	if err != nil {
		return errors.New("native editing response unknown; query the original intent")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(data) > 65536 {
		return errors.New("unconfirmed native editing response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &failure)
		if failure.Code == "NOT_FOUND" && operation == "status" {
			return errIntentNotFound
		}
		return fmt.Errorf("native editing refused (HTTP %d)", response.StatusCode)
	}
	if err = json.Unmarshal(data, result); err != nil {
		return errors.New("invalid native editing receipt")
	}
	return nil
}

var errIntentNotFound = errors.New("intent not found")

func (t *SessionHTTPTransport) command(ctx context.Context, operation, key string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return t.send(ctx, operation, "application/json", key, bytes.NewReader(data), result)
}
func (t *SessionHTTPTransport) Status(ctx context.Context) (Guard, error) {
	var result Guard
	err := t.command(ctx, "status", "", map[string]any{"reference": t.reference}, &result)
	return result, err
}
func proofBody(ref Reference, lease Lease) map[string]any {
	return map[string]any{"reference": ref, "guard_id": lease.GuardID, "generation": lease.Generation,
		"credential_epoch": lease.CredentialEpoch, "token": lease.Token}
}
func (t *SessionHTTPTransport) Checkout(ctx context.Context, base string) (Lease, error) {
	if !objectID.MatchString(base) {
		return Lease{}, errors.New("explicit native baseline required")
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		return Lease{}, err
	}
	key, err := newIntentID()
	if err != nil {
		return Lease{}, err
	}
	var result struct {
		Receipt Guard `json:"receipt"`
	}
	err = t.command(ctx, "checkout", key, map[string]any{"reference": t.reference, "base_file_id": base, "token": hex.EncodeToString(token)}, &result)
	if err != nil {
		return Lease{}, err
	}
	if !result.Receipt.Active || result.Receipt.Mode != "checkout" || result.Receipt.BaseFileID != base {
		return Lease{}, errors.New("checkout is not confirmed")
	}
	result.Receipt.Token = hex.EncodeToString(token)
	return result.Receipt.Lease, nil
}

// Prepare verifies the persisted local snapshot and current Checkout. The
// current HTTP contract performs durable Server Prepare inside commit-file;
// it deliberately exposes no separate public Prepare route.
func (t *SessionHTTPTransport) Prepare(ctx context.Context, lease Lease, intent Intent) error {
	if intent.Action != "checkin-unchanged" {
		input, _, err := verifiedSnapshot(intent)
		if err != nil {
			return err
		}
		input.Close()
	}
	current, err := t.Status(ctx)
	if err != nil {
		return err
	}
	if !current.Active || current.Mode != "checkout" || current.GuardID != lease.GuardID || current.Generation != lease.Generation ||
		current.CredentialEpoch != lease.CredentialEpoch || current.BaseFileID != intent.ExpectedFileID ||
		(current.PendingIntent != nil && *current.PendingIntent != intent.ID) {
		return errors.New("checkout changed; resolve the pending intent")
	}
	return nil
}
func verifiedSnapshot(intent Intent) (*os.File, int64, error) {
	if !canonicalUUID.MatchString(intent.ID) || !objectID.MatchString(intent.ExpectedFileID) {
		return nil, 0, errors.New("invalid intent identity")
	}
	info, err := os.Lstat(intent.Snapshot)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSnapshotBytes {
		return nil, 0, errors.New("bounded immutable snapshot required")
	}
	input, err := os.Open(intent.Snapshot)
	if err != nil {
		return nil, 0, err
	}
	opened, err := input.Stat()
	if err != nil || !os.SameFile(info, opened) {
		input.Close()
		return nil, 0, errors.New("snapshot identity changed")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(input, maxSnapshotBytes+1))
	if err != nil || count != info.Size() || hex.EncodeToString(hash.Sum(nil)) != intent.Digest {
		input.Close()
		return nil, 0, errors.New("snapshot content changed")
	}
	_, err = input.Seek(0, io.SeekStart)
	if err != nil {
		input.Close()
		return nil, 0, err
	}
	return input, count, nil
}
func (t *SessionHTTPTransport) Publish(ctx context.Context, lease Lease, intent Intent) (Receipt, error) {
	var result struct {
		Intent Receipt `json:"intent"`
	}
	head, _, err := t.version(ctx)
	if err != nil || !objectID.MatchString(head) {
		return Receipt{}, errors.New("current native head unavailable")
	}
	fields := map[string]string{"repo_id": t.reference.RepoID, "path": t.reference.Path, "guard_id": lease.GuardID,
		"generation": lease.Generation, "credential_epoch": lease.CredentialEpoch, "token": lease.Token,
		"intent_id": intent.ID, "base_file_id": intent.ExpectedFileID, "head_id": head}
	if intent.Action == "checkin-unchanged" {
		err = t.command(ctx, "checkin", intent.ID, fields, &result)
		return result.Intent, err
	}
	if intent.Action != "commit" && intent.Action != "checkin" {
		return Receipt{}, errors.New("explicit commit or checkin required")
	}
	input, size, err := verifiedSnapshot(intent)
	if err != nil {
		return Receipt{}, err
	}
	defer input.Close()
	// Materialize multipart before any network call, then verify the exact bytes
	// copied into it. A changing work file or snapshot cannot broaden consent.
	staged, err := os.CreateTemp(filepath.Dir(intent.Snapshot), ".edit-upload-*")
	if err != nil {
		return Receipt{}, err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	writer := multipart.NewWriter(staged)
	fields["action"] = intent.Action
	fields["source_identity"] = intent.SourceIdentity
	if fields["source_identity"] == "" {
		fields["source_identity"] = filepath.Base(intent.Snapshot)
	}
	for key, value := range fields {
		if err = writer.WriteField(key, value); err != nil {
			return Receipt{}, err
		}
	}
	part, err := writer.CreateFormFile("file", filepath.Base(intent.Snapshot))
	if err != nil {
		return Receipt{}, err
	}
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(part, hash), io.LimitReader(input, maxSnapshotBytes+1))
	if err != nil || count != size || hex.EncodeToString(hash.Sum(nil)) != intent.Digest {
		return Receipt{}, errors.New("snapshot changed before upload")
	}
	if err = writer.Close(); err != nil {
		return Receipt{}, err
	}
	if err = staged.Sync(); err != nil {
		return Receipt{}, err
	}
	if _, err = staged.Seek(0, io.SeekStart); err != nil {
		return Receipt{}, err
	}
	err = t.send(ctx, "commit-file", writer.FormDataContentType(), intent.ID, staged, &result)
	return result.Intent, err
}
func (t *SessionHTTPTransport) Query(ctx context.Context, intent Intent) (Receipt, error) {
	var result Receipt
	err := t.command(ctx, "status", "", map[string]any{"reference": t.reference, "intent_id": intent.ID}, &result)
	if errors.Is(err, errIntentNotFound) {
		return Receipt{IntentID: intent.ID, State: "not-found"}, nil
	}
	return result, err
}
func (t *SessionHTTPTransport) Heartbeat(ctx context.Context, lease Lease) error {
	key, err := newIntentID()
	if err != nil {
		return err
	}
	var result struct {
		Receipt Guard `json:"receipt"`
	}
	if err = t.command(ctx, "heartbeat", key, proofBody(t.reference, lease), &result); err != nil {
		return err
	}
	if !result.Receipt.Active || result.Receipt.GuardID != lease.GuardID || result.Receipt.Generation != lease.Generation || result.Receipt.CredentialEpoch != lease.CredentialEpoch {
		return errors.New("heartbeat not confirmed")
	}
	return nil
}
func (t *SessionHTTPTransport) Abandon(ctx context.Context, lease Lease) error {
	key, err := newIntentID()
	if err != nil {
		return err
	}
	var result struct {
		Receipt Guard `json:"receipt"`
	}
	if err = t.command(ctx, "abandon", key, proofBody(t.reference, lease), &result); err != nil {
		return err
	}
	if result.Receipt.Active || result.Receipt.Generation != lease.Generation {
		return errors.New("abandon is not confirmed")
	}
	return nil
}
func (t *SessionHTTPTransport) Resume(ctx context.Context, lease Lease) (Lease, error) {
	key, err := newIntentID()
	if err != nil {
		return Lease{}, err
	}
	token := make([]byte, 32)
	if _, err = rand.Read(token); err != nil {
		return Lease{}, err
	}
	body := proofBody(t.reference, lease)
	body["token"] = hex.EncodeToString(token)
	body["base_file_id"] = lease.BaseFileID
	var result struct {
		Receipt Guard `json:"receipt"`
	}
	if err = t.command(ctx, "resume", key, body, &result); err != nil {
		return Lease{}, err
	}
	previous, oldErr := strconv.ParseUint(lease.CredentialEpoch, 10, 64)
	next, newErr := strconv.ParseUint(result.Receipt.CredentialEpoch, 10, 64)
	if oldErr != nil || newErr != nil || previous == ^uint64(0) || next != previous+1 || !result.Receipt.Active || result.Receipt.Mode != "checkout" || result.Receipt.GuardID != lease.GuardID ||
		result.Receipt.Generation != lease.Generation || result.Receipt.BaseFileID != lease.BaseFileID {
		return Lease{}, errors.New("resumed credential not confirmed")
	}
	result.Receipt.Token = hex.EncodeToString(token)
	return result.Receipt.Lease, nil
}

// DownloadWorkingCopy obtains an OIDC read ticket, then uses the guarded native
// byte route with no session cookie. Endpoints must be explicitly same-origin;
// no legacy /files token or ordinary update-api fallback is allowed.
func (t *SessionHTTPTransport) DownloadWorkingCopy(ctx context.Context, lease Lease, ticketEndpoint, readEndpoint, destination string, maxBytes int64) error {
	hub, _ := url.Parse(t.endpoint)
	ticketURL, e1 := url.Parse(ticketEndpoint)
	readURL, e2 := url.Parse(readEndpoint)
	if e1 != nil || e2 != nil || maxBytes <= 0 || maxBytes > maxSnapshotBytes || ticketURL.User != nil || readURL.User != nil ||
		ticketURL.Scheme != hub.Scheme || ticketURL.Host != hub.Host || readURL.Scheme != hub.Scheme || readURL.Host != hub.Host ||
		ticketURL.RawQuery != "" || readURL.RawQuery != "" || ticketURL.Fragment != "" || readURL.Fragment != "" ||
		!strings.HasSuffix(ticketURL.Path, "/extensions/identity/v1/read-tickets/") || !strings.HasSuffix(readURL.Path, "/cloudfile/read") {
		return errors.New("trusted native read endpoints and bounded working copy required")
	}
	_, before, err := t.version(ctx)
	if err != nil || before != lease.BaseFileID {
		return errors.New("checkout baseline changed before download")
	}
	body, _ := json.Marshal(map[string]any{"reference": t.reference, "operation": "download"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, ticketEndpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := t.client.Do(request)
	if err != nil {
		return errors.New("read ticket unavailable")
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
	response.Body.Close()
	var grant struct {
		Ticket string `json:"ticket"`
	}
	if readErr != nil || len(data) > 65536 || response.StatusCode != 201 || json.Unmarshal(data, &grant) != nil || !canonicalUUID.MatchString(grant.Ticket) {
		return errors.New("read ticket unconfirmed")
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodGet, readEndpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+grant.Ticket)
	bareClient := *t.client
	bareClient.Jar = nil
	response, err = bareClient.Do(request)
	if err != nil {
		return errors.New("working copy download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("guarded download refused")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		output.Close()
		if !accepted {
			os.Remove(destination)
		}
	}()
	count, err := io.Copy(output, io.LimitReader(response.Body, maxBytes+1))
	if err != nil || count > maxBytes {
		return errors.New("working copy incomplete or too large")
	}
	if response.ContentLength >= 0 && response.ContentLength != count {
		return errors.New("working copy length mismatch")
	}
	_, after, err := t.version(ctx)
	if err != nil || after != lease.BaseFileID {
		return errors.New("checkout baseline drift during download")
	}
	if err = output.Sync(); err != nil {
		return err
	}
	if err = output.Close(); err != nil {
		return err
	}
	accepted = true
	return nil
}

func (t *SessionHTTPTransport) CancelPrepared(ctx context.Context, lease Lease, intent Intent) (Receipt, error) {
	body := proofBody(t.reference, lease)
	body["intent_id"] = intent.ID
	var result struct {
		Receipt Receipt `json:"receipt"`
	}
	if err := t.command(ctx, "cancel", intent.ID+"-cancel", body, &result); err != nil {
		return Receipt{}, err
	}
	if result.Receipt.IntentID != intent.ID || result.Receipt.State != "cancelled" {
		return Receipt{}, errors.New("intent cancellation not confirmed")
	}
	return result.Receipt, nil
}
