package editing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

// CreateSnapshot captures one regular file for an explicit manual submission.
// It never renames, truncates or removes the working copy. Multi-file projects
// and application-specific save completion are outside this single-file path.
func CreateSnapshot(source, privateDir string, maxBytes int64) (path, digest string, err error) {
	if maxBytes <= 0 {
		return "", "", errors.New("positive snapshot size limit required")
	}
	before, err := os.Lstat(source)
	if err != nil {
		return "", "", err
	}
	if !before.Mode().IsRegular() || before.Size() > maxBytes {
		return "", "", errors.New("bounded regular file required")
	}
	input, err := os.Open(source)
	if err != nil {
		return "", "", err
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", "", errors.New("working file changed before snapshot")
	}
	output, err := os.CreateTemp(privateDir, ".cloudfile-snapshot-*")
	if err != nil {
		return "", "", err
	}
	path = output.Name()
	defer func() {
		output.Close()
		if err != nil {
			os.Remove(path)
		}
	}()
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, maxBytes+1))
	if err != nil {
		return path, "", err
	}
	if count > maxBytes {
		return path, "", errors.New("snapshot exceeds size limit")
	}
	after, err := os.Stat(source)
	if err != nil {
		return path, "", err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return path, "", errors.New("working file changed during snapshot; save and retry")
	}
	digest = hex.EncodeToString(hash.Sum(nil))
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return path, "", err
	}
	verify := sha256.New()
	count, err = io.Copy(verify, io.LimitReader(input, maxBytes+1))
	if err != nil {
		return path, "", err
	}
	if count > maxBytes || hex.EncodeToString(verify.Sum(nil)) != digest {
		return path, "", errors.New("working file is still changing")
	}
	if err = output.Chmod(0400); err != nil {
		return path, "", err
	}
	if err = output.Sync(); err != nil {
		return path, "", err
	}
	if err = output.Close(); err != nil {
		return path, "", err
	}
	ready := path + ".ready"
	if err = renameJournal(path, ready); err != nil {
		os.Remove(ready)
		return path, "", err
	}
	path = ready
	return path, digest, nil
}

// SnapshotDetails persists consent to one capture. SourceIdentity is an opaque
// local work-copy identity; raw workstation paths are not sent to the server.
type SnapshotDetails struct {
	Path           string
	Digest         string
	Size           int64
	SourceIdentity string
	CreatedAt      string
}

func CaptureSnapshot(source, privateDir string, maxBytes int64) (SnapshotDetails, error) {
	path, digest, err := CreateSnapshot(source, privateDir, maxBytes)
	if err != nil {
		return SnapshotDetails{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return SnapshotDetails{}, err
	}
	absolute, err := filepath.Abs(source)
	if err != nil {
		return SnapshotDetails{}, err
	}
	identity := sha256.Sum256([]byte(absolute))
	return SnapshotDetails{Path: path, Digest: digest, Size: info.Size(), SourceIdentity: "workcopy:" + hex.EncodeToString(identity[:]), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
}
