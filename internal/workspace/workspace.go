// Package workspace keeps view and editable copies separate without
// introducing a Seafile sync client or overwriting untracked user files.
package workspace

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/config"
)

const (
	ModeView = "local-view"
	ModeEdit = "local-edit"
)

var repositoryID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type Record struct {
	RepoID   string `json:"repo_id"`
	Path     string `json:"path"`
	Mode     string `json:"mode"`
	Name     string `json:"name"`
	FileID   string `json:"file_id"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	StoredAt int64  `json:"stored_at"`
}

// Resolve returns the mirror root for use, carrying an existing mirror over
// when the resolved root has changed (see Adopt).  A failed move is
// deliberately not an error: the caller continues against the resolved root
// and the files stay where they were, which is always better than refusing to
// open a session.
func Resolve(cfg config.Config) (string, error) {
	root, err := cfg.Root()
	if err != nil {
		return "", err
	}
	_, _ = Adopt(root)
	return root, nil
}
func validMode(mode string) bool { return mode == ModeEdit || mode == ModeView }

// RelPath validates a canonical Seafile library path. Local mirrors are
// intentionally flat, so only the last file name becomes a disk entry.
func RelPath(path string) string {
	if !strings.HasPrefix(path, "/") || path == "/" || strings.ContainsAny(path, "\\\x00") || strings.Contains(path, "//") {
		return ""
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return ""
		}
	}
	return strings.TrimPrefix(path, "/")
}

func Dir(root, mode, repoID string) (string, error) {
	if root == "" || !filepath.IsAbs(root) || !validMode(mode) || !repositoryID.MatchString(repoID) {
		return "", errors.New("absolute workspace root, valid mode and library required")
	}
	return filepath.Join(root, map[string]string{ModeView: "view", ModeEdit: "edit"}[mode], repoID), nil
}
func Path(root, mode, repoID, path string) (string, error) {
	dir, err := Dir(root, mode, repoID)
	if err != nil {
		return "", err
	}
	name, _ := FileName(root, mode, repoID, path)
	if name == "" {
		return "", errors.New("invalid mirror path")
	}
	return filepath.Join(dir, name), nil
}

func RecordPath(root, mode, repoID, remotePath string) (string, error) {
	if RelPath(remotePath) == "" {
		return "", errors.New("invalid canonical library path")
	}
	dir, err := Dir(root, mode, repoID)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(repoID + "\x00" + mode + "\x00" + remotePath))
	_ = dir // validate the library/mode before placing private metadata outside the visible flat mirror
	return filepath.Join(root, ".cloudfile-records", mode, repoID, hex.EncodeToString(h[:])+".json"), nil
}
func unsafeName(name string) bool {
	return strings.ContainsAny(name, `/\`) || strings.IndexByte(name, 0) >= 0
}

func ReadRecord(root, mode, repoID, remotePath string) (Record, bool) {
	path, err := RecordPath(root, mode, repoID, remotePath)
	if err != nil {
		return Record{}, false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return Record{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, false
	}
	var record Record
	if json.Unmarshal(b, &record) != nil || record.RepoID != repoID || record.Mode != mode || record.Path != remotePath ||
		record.Name == "" || record.Name == "." || record.Name == ".." || unsafeName(record.Name) || filepath.Base(record.Name) != record.Name {
		return Record{}, false
	}
	return record, true
}
func FileName(root, mode, repoID, remotePath string) (string, bool) {
	relative := RelPath(remotePath)
	if relative == "" {
		return "", false
	}
	if _, err := Dir(root, mode, repoID); err != nil {
		return "", false
	}
	if record, found := ReadRecord(root, mode, repoID, remotePath); found {
		return record.Name, true
	}
	name := filepath.Base(relative)
	if name == "." || name == "" {
		return "", false
	}
	return name, false
}
func WriteRecord(root, mode, repoID, remotePath string, record Record) error {
	target, err := RecordPath(root, mode, repoID, remotePath)
	if err != nil {
		return err
	}
	if record.RepoID != repoID || record.Path != remotePath || record.Mode != mode ||
		record.Name == "" || record.Name == "." || record.Name == ".." || unsafeName(record.Name) || filepath.Base(record.Name) != record.Name {
		return errors.New("record identity invalid")
	}
	directory := filepath.Dir(target)
	if err = os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if record.StoredAt == 0 {
		record.StoredAt = time.Now().Unix()
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(directory, ".cf-rec-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), target)
}

// UniqueName never treats a coincidental same-name file as the matching
// CloudFile copy. The original is untouched, with numeric collision suffixes.
func UniqueName(dir, name string) (string, error) {
	if name == "" || name == "." || filepath.Base(name) != name {
		return "", errors.New("unsafe file name")
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 10000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		_, err := os.Lstat(filepath.Join(dir, candidate))
		if os.IsNotExist(err) {
			return candidate, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("too many existing file names")
}
func Hashes(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	sha1Hash, sha256Hash := sha1.New(), sha256.New()
	if _, err = io.Copy(io.MultiWriter(sha1Hash, sha256Hash), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(sha1Hash.Sum(nil)), hex.EncodeToString(sha256Hash.Sum(nil)), nil
}
func DigestFile(path string) (string, error) {
	_, digest, err := Hashes(path)
	return digest, err
}
func IsReadOnly(info os.FileInfo) bool { return info.Mode().Perm()&0200 == 0 }
