package blockdelta

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// BlockStager is an authenticated, resource-scoped staging provider supplied
// by CloudFile's device broker. It MUST NOT publish a file or accept an
// unrestricted Seafile repo token. A successful Put only stores raw blocks;
// the native Editing Core remains the sole guarded publisher.
type BlockStager interface {
	Missing(ctx context.Context, orderedIDs []string) ([]string, error)
	Put(ctx context.Context, id string, raw []byte) error
}

type Staged struct {
	Manifest       Manifest
	MissingBlocks  int
	UploadedBlocks int
	UploadedBytes  int64
}

// StageSnapshotOnly stages missing, non-encrypted Seafile CDC blocks. It never
// makes a file visible and never creates a CommitIntent; a future guarded
// CommitBlocks endpoint must perform those operations after reconfirming
// actor/device, base version, guard, and idempotent intent.
// The caller must supply an immutable snapshot and an explicitly authorized
// per-resource BlockStager. No automatic upload is wired to local saves.
func StageSnapshotOnly(ctx context.Context, snapshotPath string, repoVersion int, stager BlockStager) (Staged, error) {
	if stager == nil {
		return Staged{}, errors.New("authorized block stager required")
	}
	manifest, err := ScanSnapshot(snapshotPath, repoVersion)
	if err != nil {
		return Staged{}, err
	}
	ids := make([]string, 0, len(manifest.Blocks))
	unique := make(map[string]Block, len(manifest.Blocks))
	for _, b := range manifest.Blocks {
		ids = append(ids, b.ID)
		if _, exists := unique[b.ID]; !exists {
			unique[b.ID] = b
		}
	}
	// Empty file still requires a guarded zero-byte commit; do not stage.
	if len(ids) == 0 {
		return Staged{Manifest: manifest}, nil
	}
	missing, err := stager.Missing(ctx, ids)
	if err != nil {
		return Staged{}, err
	}
	seen := make(map[string]bool, len(missing))
	for _, id := range missing {
		if _, ok := unique[id]; !ok || seen[id] {
			return Staged{}, errors.New("untrusted missing-block response")
		}
		seen[id] = true
	}
	f, err := os.Open(snapshotPath)
	if err != nil {
		return Staged{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != manifest.Size {
		return Staged{}, errors.New("snapshot invalid before staging")
	}
	result := Staged{Manifest: manifest, MissingBlocks: len(missing)}
	for _, id := range missing {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		block := unique[id]
		if block.Size <= 0 || block.Size > MaxSize {
			return result, errors.New("invalid block length")
		}
		payload := make([]byte, int(block.Size))
		n, err := f.ReadAt(payload, block.Offset)
		if err != nil && err != io.EOF {
			return result, err
		}
		if int64(n) != block.Size {
			return result, errors.New("snapshot block truncated")
		}
		sum := sha1.Sum(payload)
		if hex.EncodeToString(sum[:]) != id {
			return result, errors.New("snapshot block changed before upload")
		}
		if err = stager.Put(ctx, id, payload); err != nil {
			return result, err
		}
		result.UploadedBlocks++
		result.UploadedBytes += block.Size
	}
	// Verify the entire original snapshot a second time. Blocks not uploaded
	// because already present must still match the signed/intended manifest.
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return result, err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(f, manifest.Size+1))
	if err != nil || n != manifest.Size || hex.EncodeToString(digest.Sum(nil)) != manifest.SHA256 {
		return result, errors.New("snapshot changed while staging; never publish")
	}
	return result, nil
}
