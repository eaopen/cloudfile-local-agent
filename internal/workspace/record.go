package workspace

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Record is the download fingerprint of one mirrored file.
//
// It answers the only two questions the "local edit with an existing local
// file" decision needs: which server version was downloaded, and what the local
// file looked like at that moment. Comparing it against the version the server
// is serving now, and against the file on disk now, tells the caller whether the
// server moved on, whether the user changed the local copy, both, or neither.
//
// The fingerprint lives on the machine next to the mirror instead of in browser
// storage, so clearing the browser cache, switching browser or moving the mirror
// to another drive does not silently reset the comparison.
type Record struct {
	RepoID string `json:"repo_id"`
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	// Name is the file name this copy carries inside its flat library folder.
	// It is recorded because the mirror does not reproduce the library tree:
	// two library paths that share a file name cannot both use it, so the
	// second one is stored as "name (1)". This field is what makes the
	// in-library path, rather than the on-disk name, the identity of a file.
	Name string `json:"name,omitempty"`
	// FileID is the Seafile object ID (obj_id) of the downloaded version. It is
	// content-addressed, so it changes whenever the file's content changes. It
	// is deliberately never compared against a content hash: the two are
	// different quantities and would never match.
	FileID string `json:"file_id,omitempty"`
	// Digest is the SHA-256 of the local copy right after the download.
	Digest string `json:"digest,omitempty"`
	// Size is the local size right after the download, in bytes.
	Size int64 `json:"size,omitempty"`
	// StoredAt is the Unix time the fingerprint was written.
	StoredAt int64 `json:"stored_at"`
}

// ReadRecord loads the fingerprint for one mirrored file. A missing or corrupt
// file is reported as "no fingerprint" rather than an error: the caller then
// treats the local copy as unverifiable, which is the conservative answer.
func ReadRecord(root, mode, repoID, serverPath string) (Record, bool) {
	path, err := metaPath(root, mode, repoID, serverPath)
	if err != nil {
		return Record{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, false
	}
	record := Record{}
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, false
	}
	return record, true
}

// WriteRecord stores a fingerprint, replacing any earlier one. The write goes
// through a temporary file in the same directory so a reader never sees a
// half-written record.
func WriteRecord(root, mode, repoID, serverPath string, record Record) error {
	path, err := metaPath(root, mode, repoID, serverPath)
	if err != nil {
		return err
	}
	// Name makes the record addressable as a local file, so it is validated the
	// same way a download name is: a record that pointed outside the library
	// folder would turn a lookup into a write somewhere else.
	if record.Name != "" {
		name, err := validateFileName(record.Name)
		if err != nil {
			return err
		}
		record.Name = name
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if record.StoredAt == 0 {
		record.StoredAt = time.Now().Unix()
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		os.Remove(name)
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// DigestFile returns the lowercase hex SHA-256 of a file's contents.
func DigestFile(path string) (string, error) {
	_, digest, err := Hashes(path)
	return digest, err
}

// Hashes returns the SHA-1 and SHA-256 of a file's contents in a single read,
// so the legacy SHA-1 reported to the web page costs no extra pass over the
// file.
func Hashes(path string) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	sha1Hasher := sha1.New()
	sha256Hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(sha1Hasher, sha256Hasher), file); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(sha1Hasher.Sum(nil)), hex.EncodeToString(sha256Hasher.Sum(nil)), nil
}

// metaPath returns the fingerprint location for one mirrored file:
//
//	{root}/.cloudfile-meta/{view|edit}/{repo_id}/{path}.json
//
// It mirrors the file's own relative path, so two files with the same name in
// different folders never share a fingerprint.
func metaPath(root, mode, repoID, serverPath string) (string, error) {
	if err := validateSegment(repoID); err != nil {
		return "", err
	}
	rel := RelPath(serverPath)
	if rel == "" {
		return "", fmt.Errorf("fingerprints are per file")
	}
	return filepath.Join(root, MetaDir, Subdir(mode), repoID, rel+".json"), nil
}

// IsReadOnly reports whether a mirrored file carries the read-only marker that
// view downloads are given. It is the only reliable way to tell a legacy view
// copy from a legacy edit copy during migration.
func IsReadOnly(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0200 == 0
}

// sameContent reports whether two regular files hold identical bytes.
func sameContent(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false, err
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false, err
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}
	leftDigest, err := DigestFile(left)
	if err != nil {
		return false, err
	}
	rightDigest, err := DigestFile(right)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(leftDigest, rightDigest), nil
}
