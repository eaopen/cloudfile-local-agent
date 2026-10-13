package blockdelta

import (
	"context"
	"errors"
	"os"
	"testing"
)

type stagingFake struct {
	missing  []string
	expected map[string]bool
	stored   map[string][]byte
	fail     bool
	onPut    func()
}

func (s *stagingFake) Missing(_ context.Context, ids []string) ([]string, error) {
	if s.expected == nil {
		s.expected = map[string]bool{}
	}
	for _, id := range ids {
		s.expected[id] = true
	}
	return s.missing, nil
}
func (s *stagingFake) Put(_ context.Context, id string, raw []byte) error {
	if s.fail {
		return errors.New("transport refused")
	}
	if !s.expected[id] {
		return errors.New("unapproved block")
	}
	if s.stored == nil {
		s.stored = make(map[string][]byte)
	}
	s.stored[id] = append([]byte(nil), raw...)
	if s.onPut != nil {
		s.onPut()
	}
	return nil
}
func TestStageOnlyMissingNoPublication(t *testing.T) {
	path := writeReferenceFile(t)
	m, err := ScanSnapshot(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	// One missing block of four: approximately 8 MiB uploaded instead of 31 MiB.
	store := &stagingFake{missing: []string{m.Blocks[1].ID}}
	result, err := StageSnapshotOnly(context.Background(), path, 1, store)
	if err != nil {
		t.Fatal(err)
	}
	if result.UploadedBlocks != 1 || result.MissingBlocks != 1 || result.UploadedBytes != m.Blocks[1].Size {
		t.Fatalf("unexpected delta stats %+v", result)
	}
	if len(store.stored) != 1 || int64(len(store.stored[m.Blocks[1].ID])) != m.Blocks[1].Size {
		t.Fatal("wrong payload")
	}
	if result.Manifest.FileID != m.FileID {
		t.Fatal("manifest changed")
	}
	// Reused server blocks require no outbound payload at all.
	empty, err := StageSnapshotOnly(context.Background(), path, 1, &stagingFake{})
	if err != nil || empty.UploadedBytes != 0 {
		t.Fatalf("expected no-op staging %+v %v", empty, err)
	}
}
func TestRejectUntrustedMissingAndFailedStage(t *testing.T) {
	path := writeReferenceFile(t)
	m, err := ScanSnapshot(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]string{{"ffffffffffffffffffffffffffffffffffffffff"}, {m.Blocks[0].ID, m.Blocks[0].ID}} {
		store := &stagingFake{missing: ids}
		if _, err := StageSnapshotOnly(context.Background(), path, 1, store); err == nil {
			t.Fatalf("accepted invalid missing response %v", ids)
		}
		if len(store.stored) != 0 {
			t.Fatal("must not upload untrusted block")
		}
	}
	store := &stagingFake{missing: []string{m.Blocks[0].ID}, fail: true}
	if _, err := StageSnapshotOnly(context.Background(), path, 1, store); err == nil {
		t.Fatal("silent upload failure")
	}
	if _, err := StageSnapshotOnly(context.Background(), path, 1, nil); err == nil {
		t.Fatal("stager required")
	}
}
func TestSnapshotChangedWhileStage(t *testing.T) {
	path := writeReferenceFile(t)
	m, err := ScanSnapshot(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	store := &stagingFake{missing: []string{m.Blocks[0].ID}, onPut: func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.WriteAt([]byte{0xff}, m.Blocks[2].Offset+99)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := StageSnapshotOnly(context.Background(), path, 1, store); err == nil {
		t.Fatal("accepted changed snapshot")
	}
}
