package blockdelta

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// MissingQuery must be implemented by CloudFile's authenticated device broker,
// returning a subset of IDs for one permitted repo/file/checkout.
// A normal Seafile token must never be promoted to an editing credential.
type MissingQuery func(context.Context, []string) ([]string, error)

// HTTPSRawBlockStager is only a data transport: it can upload missing raw
// Seafile blocks but cannot publish any file or release a checkout. Its
// short-lived upload URL must be provided by an independently authorized
// CloudFile broker. No automatic save watcher calls it.
type HTTPSRawBlockStager struct {
	client  *http.Client
	url     string
	missing MissingQuery
}

var rawUploadPath = regexp.MustCompile(`(?:^|/)upload-raw-blks-api/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/?$`)
var blockHash = regexp.MustCompile(`^[0-9a-f]{40}$`)

// trustedOrigin is the already-paired CloudFile HTTPS origin. External or
// cross-origin Seafile upload URLs are rejected even if server-provided.
func NewHTTPSRawBlockStager(rawURL, trustedOrigin string, source *http.Client, missing MissingQuery) (*HTTPSRawBlockStager, error) {
	u, err := url.Parse(rawURL)
	origin, originErr := url.Parse(trustedOrigin)
	if err != nil || originErr != nil || source == nil || missing == nil ||
		(u.Scheme != "https" && !device.AllowHTTPOrigin(u.Scheme+"://"+u.Host)) || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(origin.Scheme != "https" && !device.AllowHTTPOrigin(origin.Scheme+"://"+origin.Host)) || origin.Host == "" || origin.User != nil || origin.Path != "" ||
		origin.RawQuery != "" || origin.Fragment != "" || u.Scheme != origin.Scheme || !strings.EqualFold(u.Host, origin.Host) ||
		!rawUploadPath.MatchString(u.EscapedPath()) || strings.Contains(u.EscapedPath(), "%") {
		return nil, errors.New("paired same-origin, scoped same-origin HTTP/HTTPS raw-block URL required")
	}
	copyClient := *source
	copyClient.Jar = nil
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copyClient.Timeout == 0 || copyClient.Timeout > 3*time.Minute {
		copyClient.Timeout = 3 * time.Minute
	}
	if tr, ok := copyClient.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil && tr.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("verified TLS required for block upload")
	}
	return &HTTPSRawBlockStager{client: &copyClient, url: rawURL, missing: missing}, nil
}
func (s *HTTPSRawBlockStager) Missing(ctx context.Context, ids []string) ([]string, error) {
	return s.missing(ctx, ids)
}
func (s *HTTPSRawBlockStager) Put(ctx context.Context, id string, raw []byte) error {
	if !blockHash.MatchString(id) || len(raw) < 1 || len(raw) > MaxSize {
		return errors.New("invalid raw block size or identity")
	}
	sum := sha1.Sum(raw)
	if hex.EncodeToString(sum[:]) != id {
		return errors.New("raw block digest changed")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", id)
	if err != nil {
		return err
	}
	if _, err = part.Write(raw); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, &body)
	if err != nil {
		return errors.New("invalid upload request")
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return errors.New("raw block upload result unknown; query missing blocks before retry")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil || resp.StatusCode != http.StatusOK || strings.TrimSpace(string(data)) != `"OK"` {
		return errors.New("raw block staging not confirmed")
	}
	return nil
}

// Only publish through CloudFile EditingService after these staged blocks
// have been verified against the guarded CommitIntent and server-side ACL.
// This type intentionally has no Commit, Checkin, or file download methods.
var _ BlockStager = (*HTTPSRawBlockStager)(nil)
