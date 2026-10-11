package nativehost

import "testing"

func TestOpenURICannotCarryArbitraryLocalPathOrDownloadCapabilities(t *testing.T) {
	uri := "cloudfile-open://v1/open?instance=https%3A%2F%2Fcloudfile.example&session=s&ticket=t"
	if !(Request{Type: "open_uri", URI: uri}).Valid() {
		t.Fatal("native host did not accept bounded URI envelope")
	}
	for _, bad := range []Request{
		{Type: "open_uri", URI: uri, Path: "/private/passwd"},
		{Type: "open_uri", URI: uri, Ticket: "unauthorized"},
		{Type: "open_uri", URI: uri, Server: "https://bad.example"},
		{Type: "open_uri", URI: uri, Mode: "local-edit"},
		{Type: "open_uri", URI: uri, RepoID: "foreign"},
		{Type: "open_uri", URI: ""},
	} {
		if bad.Valid() {
			t.Fatal("confused URI with arbitrary host action")
		}
	}
}
