package blockdelta

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func verifiedTestClient(server *httptest.Server) *http.Client {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
}
func TestRawBlockHTTPSStaging(t *testing.T) {
	saved := map[string]string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/seafhttp/upload-raw-blks-api/12345678-1234-1234-1234-123456789abc" {
			t.Errorf("invalid upload request")
			http.Error(w, "refused", 400)
			return
		}
		if err := r.ParseMultipartForm(12 << 20); err != nil {
			t.Errorf("invalid multipart %v", err)
			http.Error(w, "refused", 400)
			return
		}
		files := r.MultipartForm.File["file"]
		if len(files) != 1 {
			t.Errorf("expected one raw block")
			http.Error(w, "refused", 400)
			return
		}
		f, err := files[0].Open()
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		payload, err := io.ReadAll(f)
		if err != nil {
			t.Error(err)
			return
		}
		saved[files[0].Filename] = string(payload)
		_, _ = w.Write([]byte(`"OK"`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "workcopy")
	if err := os.WriteFile(path, []byte("Seafile block upload via scoped HTTPS"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := ScanSnapshot(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	stager, err := NewHTTPSRawBlockStager(
		server.URL+"/seafhttp/upload-raw-blks-api/12345678-1234-1234-1234-123456789abc",
		server.URL, verifiedTestClient(server), func(_ context.Context, ids []string) ([]string, error) {
			if len(ids) != 1 || ids[0] != m.Blocks[0].ID {
				t.Fatal("unexpected block negotiation")
			}
			return ids, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	got, err := StageSnapshotOnly(context.Background(), path, 1, stager)
	if err != nil {
		t.Fatal(err)
	}
	if got.UploadedBlocks != 1 || got.UploadedBytes != int64(len("Seafile block upload via scoped HTTPS")) {
		t.Fatalf("wrong stats %+v", got)
	}
	if saved[m.Blocks[0].ID] != "Seafile block upload via scoped HTTPS" {
		t.Fatal("wrong uploaded block bytes")
	}
}
func TestRawBlockHTTPSRefusesUnsafeDestination(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://attacker.invalid/")
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	cb := func(context.Context, []string) ([]string, error) { return nil, nil }
	good := server.URL + "/seafhttp/upload-raw-blks-api/12345678-1234-1234-1234-123456789abc"
	for _, bad := range []string{
		"http://example.com/upload-raw-blks-api/12345678-1234-1234-1234-123456789abc",
		"https://outside.invalid/upload-raw-blks-api/12345678-1234-1234-1234-123456789abc",
		server.URL + "/seafhttp/upload-raw-blks-api/12345678-1234-1234-1234-123456789abc?origin=bad",
		server.URL + "/seafhttp/upload-raw-blks-api/12345678-1234-1234-1234-123456789abc/../../bad",
		server.URL + "/update-api/12345678-1234-1234-1234-123456789abc",
	} {
		if _, err := NewHTTPSRawBlockStager(bad, server.URL, verifiedTestClient(server), cb); err == nil {
			t.Errorf("accepted unsafe endpoint %s", bad)
		}
	}
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if _, err := NewHTTPSRawBlockStager(good, server.URL, insecure, cb); err == nil {
		t.Fatal("accepted TLS verification bypass")
	}
	stager, err := NewHTTPSRawBlockStager(good, server.URL, verifiedTestClient(server), cb)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("abc")
	if err := stager.Put(context.Background(), "a9993e364706816aba3e25717850c26c9cd0d89d", payload); err == nil {
		t.Fatal("followed redirect or falsely acknowledged staging")
	}
	if err := stager.Put(context.Background(), "a9993e364706816aba3e25717850c26c9cd0d89d", []byte("wrong")); err == nil {
		t.Fatal("accepted mismatched digest")
	}
	if err := stager.Put(context.Background(), fmt.Sprint("bad"), payload); err == nil {
		t.Fatal("accepted invalid block id")
	}
}
