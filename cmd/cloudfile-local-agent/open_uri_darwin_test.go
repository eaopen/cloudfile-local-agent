//go:build darwin && cgo

package main

import (
	"github.com/eaopen/cloudfile-local-agent/internal/device"
	"net/url"
	"strings"
	"testing"
)

func TestPairedOpenURIIsCanonicalAndCannotNominateOtherData(t *testing.T) {
	origin := "https://cloudfile.example"
	sid := "11111111-1111-4111-8111-111111111111"
	token := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	base := "cloudfile-open://v1/open?instance=" + url.QueryEscape(origin) + "&session=" + sid + "&ticket=" + token
	parsed, err := parsePairedOpenURI(base)
	if err != nil || parsed.Instance != origin || parsed.SessionID != sid || parsed.Ticket != token {
		t.Fatalf("valid URI rejected %v", err)
	}
	for _, attack := range []string{
		base + "&file=/etc/passwd", strings.Replace(base, "https%3A", "http%3A", 1),
		strings.Replace(base, "ticket=", "ticket=changed", 1),
		strings.Replace(base, "session=", "session=other", 1),
		strings.Replace(base, "cloudfile-open://v1", "https://v1", 1),
		strings.Replace(base, "&ticket=", "&ticket=abc&ticket=", 1),
		base + "%00",
	} {
		if _, err := parsePairedOpenURI(attack); err == nil {
			t.Fatalf("untrusted URI accepted")
		}
	}
	if device.Origin(parsed.Instance) != nil {
		t.Fatal("confirmed HTTPS origin rejected")
	}
}
