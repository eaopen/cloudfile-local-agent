package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const Protocol = "cloudfile-local/v2"

type Descriptor struct {
	Protocol  string `json:"protocol"`
	Server    string `json:"server"`
	Ticket    string `json:"ticket"`
	ExpiresAt int64  `json:"expires_at"`
}

type File struct {
	Name       string `json:"name"`
	ContentURL string `json:"content_url"`
}

type Writeback struct {
	ContentURL   string `json:"content_url"`
	HeartbeatURL string `json:"heartbeat_url"`
	Capability   string `json:"capability"`
}

type Claimed struct {
	SessionID string     `json:"session_id"`
	Mode      string     `json:"mode"`
	ExpiresAt int64      `json:"expires_at"`
	File      File       `json:"file"`
	Writeback *Writeback `json:"writeback,omitempty"`
}

func Read(path string) (Descriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, err
	}
	var descriptor Descriptor
	if err := json.Unmarshal(data, &descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("invalid CloudFile session file: %w", err)
	}
	if descriptor.Protocol != Protocol || descriptor.Server == "" || descriptor.Ticket == "" {
		return Descriptor{}, fmt.Errorf("unsupported CloudFile session file")
	}
	if descriptor.ExpiresAt <= time.Now().Unix() {
		return Descriptor{}, fmt.Errorf("CloudFile session file has expired")
	}
	return descriptor, nil
}

func Claim(descriptor Descriptor) (Claimed, error) {
	base, err := url.Parse(descriptor.Server)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return Claimed{}, fmt.Errorf("session server is invalid")
	}
	body, _ := json.Marshal(map[string]string{"ticket": descriptor.Ticket})
	endpoint := strings.TrimSuffix(descriptor.Server, "/") + "/api/v2.1/cloudfile/agent-sessions/claim/"
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Claimed{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return Claimed{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Claimed{}, fmt.Errorf("session claim was rejected (%d)", response.StatusCode)
	}
	var claimed Claimed
	if err := json.NewDecoder(response.Body).Decode(&claimed); err != nil {
		return Claimed{}, err
	}
	if claimed.SessionID == "" || claimed.File.ContentURL == "" || claimed.ExpiresAt <= time.Now().Unix() {
		return Claimed{}, fmt.Errorf("session claim response is invalid")
	}
	if !sameOrigin(descriptor.Server, claimed.File.ContentURL) ||
		(claimed.Writeback != nil && (!sameOrigin(descriptor.Server, claimed.Writeback.ContentURL) || !sameOrigin(descriptor.Server, claimed.Writeback.HeartbeatURL))) {
		return Claimed{}, fmt.Errorf("session claim returned an untrusted URL")
	}
	return claimed, nil
}

func sameOrigin(expected, candidate string) bool {
	a, errA := url.Parse(expected)
	b, errB := url.Parse(candidate)
	return errA == nil && errB == nil && a.Scheme == b.Scheme && a.Host == b.Host
}
