package blockdelta

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeReferenceFile(t *testing.T) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "reference.bin")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	seed := uint32(0x12345678)
	block := make([]byte, 64*1024)
	remaining := 31*1024*1024 + 1234
	for remaining > 0 {
		n := len(block)
		if n > remaining {
			n = remaining
		}
		for i := 0; i < n; i++ {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			block[i] = byte(seed)
		}
		if _, err = f.Write(block[:n]); err != nil {
			t.Fatal(err)
		}
		remaining -= n
	}
	return name
}

// Cut points and fingerprint independently derived by compiling and running
// Seafile's unchanged common/cdc/rabin-checksum.c against this xorshift input.
func TestCDCReference(t *testing.T) {
	r := newRabin()
	path := writeReferenceFile(t)
	data, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer data.Close()
	first := make([]byte, MinSize)
	if _, err = data.ReadAt(first, 0); err != nil {
		t.Fatal(err)
	}
	if got := r.fingerprint(first[MinSize-WindowSize:]); got != 2497634982 {
		t.Fatalf("rabin fingerprint %d != C reference", got)
	}
	m, err := ScanSnapshot(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	expected := []int64{8963465, 10370140, 10485760, 2687725}
	var actual []int64
	for i, b := range m.Blocks {
		actual = append(actual, b.Size)
		if i > 0 && b.Offset != m.Blocks[i-1].Offset+m.Blocks[i-1].Size {
			t.Fatal("non-contiguous blocks")
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("CDC cuts %+v != C reference %+v", actual, expected)
	}
	if m.SHA256 != "427793719edb77e8ac75b2584d4a45986c9d0c9a405a771f26229e4f21deb0d7" {
		t.Fatalf("digest differs %s", m.SHA256)
	}
	expectedIDs := []string{"032c8290d9c4d023bb1497d2bc0af9bc4e787e28", "70cb3a58614f357efb7d4459205fa9d745fefa50", "4dc1700247a96f9076bbf5f107c164b7b942a8bb", "7ab424638df89d9d9d090b08a084d6cb805a24b3"}
	for i, b := range m.Blocks {
		if b.ID != expectedIDs[i] {
			t.Fatalf("block %d checksum %s != C reference %s", i, b.ID, expectedIDs[i])
		}
	}
	if m.FileID != "4778b0b0f1f97cf4c0b5a556d173e8cd124a5fa3" || m.FileID != FileObjectID(m) {
		t.Fatal("file metadata identity differs from Seafile JSON reference")
	}
}
func TestEmptyAndSmall(t *testing.T) {
	p := filepath.Join(t.TempDir(), "small")
	if err := os.WriteFile(p, nil, 0600); err != nil {
		t.Fatal(err)
	}
	empty, err := ScanSnapshot(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Size != 0 || len(empty.Blocks) != 0 || empty.SHA256 != hex.EncodeToString(sha256.New().Sum(nil)) {
		t.Fatal("incorrect empty manifest")
	}
	if err = os.WriteFile(p, []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := ScanSnapshot(p, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blocks) != 1 || m.Blocks[0].ID != "a9993e364706816aba3e25717850c26c9cd0d89d" || m.Size != 3 {
		t.Fatalf("small file manifest %+v", m)
	}
	if _, err := ScanSnapshot(p, 0); err == nil {
		t.Fatal("unsupported repository version accepted")
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanSnapshot(link, 1); err == nil {
		t.Fatal("symlink must not be accepted")
	}
}
