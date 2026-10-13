// Package workspace owns the on-disk shape of the local mirror.
//
// The mirror is split into two subtrees that are never mixed, and inside each
// subtree every library gets a single flat folder:
//
//	{root}/view/{repo_id}/{name}   disposable read-only replica of a server file
//	{root}/edit/{repo_id}/{name}   the user's working copy
//
// {name} is a plain file name, never the server's folder path: the mirror does
// not reproduce the library's tree. A file's identity is its in-library path,
// which is what the download fingerprint records; the name on disk is only a
// label. Two different library paths that end in the same file name therefore
// sit side by side, and the second one gets the Windows-style " (1)" suffix
// (see UniqueName) - which is exactly why the name has to be looked up through
// the fingerprint instead of being derived from the path.
//
// repo_id stays one level below the mode so a library rename or move never
// changes local paths, and each mode subtree can be cleared or moved as a whole.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// ViewDir is the mirror subtree holding read-only view copies.
	ViewDir = "view"
	// EditDir is the mirror subtree holding the user's editable working copies.
	EditDir = "edit"

	// ModeView and ModeEdit are the session modes the Hub issues.
	ModeView = "local-view"
	ModeEdit = "local-edit"

	// MetaDir holds the per-file download fingerprints. It sits outside the two
	// mirror subtrees so browsing the mirror never shows the bookkeeping, and it
	// is readable by the Agent only.
	MetaDir = ".cloudfile-meta"

	// markerFile records which on-disk layout a root has already been migrated
	// to, so the one-time migration runs once per root.
	markerFile = ".cloudfile-layout"
	// legacySuffix is appended when a legacy file cannot be placed because the
	// new location already holds a different file, so nothing is ever dropped.
	legacySuffix = ".cloudfile-legacy"

	// duplicateSuffixFormat is the Windows convention for a name that is
	// already taken in a folder: report.pdf, report (1).pdf, report (2).pdf.
	duplicateSuffixFormat = " (%d)"

	// maxSegmentLength bounds a library identifier.
	maxSegmentLength = 128
	// maxFileNameLength bounds a file name. It leaves room for a " (n)" suffix
	// and the folder prefix inside the usual Windows path limit.
	maxFileNameLength = 200
)

// duplicateSuffixPattern matches the " (n)" tail Windows appends to a name that
// collided, so an already-suffixed name is not suffixed twice.
var duplicateSuffixPattern = regexp.MustCompile(` \((\d+)\)$`)

// Subdir maps a session mode to its mirror subtree. Anything that is not a view
// session is treated as an edit session, because "local-edit" is the only other
// mode the Hub issues and the editable default is the safe one to fall back to.
func Subdir(mode string) string {
	if mode == ModeView {
		return ViewDir
	}
	return EditDir
}

// RelPath sanitises a server-relative path into a path relative to the library
// folder. The caller passes the full in-library path including the file name;
// Clean folds "." and ".." away, so a hostile path cannot escape.
//
// An empty server path yields an empty result, which callers use to address the
// library folder itself rather than a file inside it.
//
// The relative path is the file's identity, not its location: the mirror is
// flat, and the name on disk comes from FileName.
func RelPath(serverPath string) string {
	cleaned := filepath.Clean(filepath.Join(string(filepath.Separator), serverPath))
	return strings.TrimPrefix(cleaned, string(filepath.Separator))
}

// Dir returns the flat folder that holds every local copy of one library in one
// mode: {root}/{view|edit}/{repo_id}.
func Dir(root, mode, repoID string) (string, error) {
	if err := validateSegment(repoID); err != nil {
		return "", err
	}
	return filepath.Join(root, Subdir(mode), repoID), nil
}

// ServerName is the file name carried by a server path, checked to be a single
// safe segment so it can be used as a flat file name. A folder path has no
// name of its own and is refused, because the flat mirror has no folders to
// address.
func ServerName(serverPath string) (string, error) {
	trimmed := strings.TrimSpace(serverPath)
	if trimmed == "" || trimmed == "/" {
		return "", fmt.Errorf("a file path is required")
	}
	if strings.HasSuffix(trimmed, "/") || strings.HasSuffix(trimmed, `\`) {
		return "", fmt.Errorf("%q is a folder, not a file", serverPath)
	}
	rel := RelPath(trimmed)
	if rel == "" {
		return "", fmt.Errorf("a file path is required")
	}
	return validateFileName(filepath.Base(rel))
}

// FileName returns the name the local copy of one server file carries inside
// its library folder, plus whether that name came from a download record.
//
// The record wins: a name may have been suffixed to avoid a collision, so the
// on-disk name cannot be derived from the server path once the file has been
// downloaded. With no record the server file name is returned, which is what a
// first download would pick - but the caller must not treat the name as proof
// that a local copy exists.
func FileName(root, mode, repoID, serverPath string) (string, bool) {
	if record, found := ReadRecord(root, mode, repoID, serverPath); found && record.Name != "" {
		return record.Name, true
	}
	name, err := ServerName(serverPath)
	if err != nil {
		return "", false
	}
	return name, false
}

// Path returns the absolute location of the local copy of one server file:
//
//	{root}/{view|edit}/{repo_id}/{name}
//
// An empty serverPath returns the library folder itself, which is what "open
// the local folder" needs.
func Path(root, mode, repoID, serverPath string) (string, error) {
	dir, err := Dir(root, mode, repoID)
	if err != nil {
		return "", err
	}
	if RelPath(serverPath) == "" {
		return dir, nil
	}
	name, _ := FileName(root, mode, repoID, serverPath)
	if name == "" {
		return "", fmt.Errorf("session returned an unusable file name")
	}
	return filepath.Join(dir, name), nil
}

// UniqueName returns the name a *new* download of name must use inside dir,
// following the Windows convention: "report.pdf", "report (1).pdf",
// "report (2).pdf".
//
// An existing " (n)" suffix is stripped from the stem first, so a taken
// "report (1).pdf" yields "report (2).pdf" rather than "report (1) (1).pdf".
func UniqueName(dir, name string) (string, error) {
	if _, err := validateFileName(name); err != nil {
		return "", err
	}
	if !exists(filepath.Join(dir, name)) {
		return name, nil
	}
	stem, extension := splitDuplicateSuffix(name)
	for index := 1; ; index++ {
		candidate := stem + fmt.Sprintf(duplicateSuffixFormat, index) + extension
		if !exists(filepath.Join(dir, candidate)) {
			return candidate, nil
		}
	}
}

// splitDuplicateSuffix splits a name into the stem Windows would count up from
// and the extension, so "report (1).pdf" becomes ("report", ".pdf").
func splitDuplicateSuffix(name string) (string, string) {
	extension := filepath.Ext(name)
	stem := strings.TrimSuffix(name, extension)
	if match := duplicateSuffixPattern.FindStringSubmatch(stem); match != nil {
		if index, err := strconv.Atoi(match[1]); err == nil && index > 0 {
			stem = stem[:len(stem)-len(match[0])]
		}
	}
	// A name that is nothing but a suffix keeps a usable stem.
	if stem == "" {
		stem = strings.TrimSuffix(name, extension)
	}
	return stem, extension
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// validateSegment rejects a value that would not be a single safe path segment.
// A leading dot is refused so a library can never collide with MetaDir or the
// layout marker.
func validateSegment(value string) error {
	if value == "" {
		return fmt.Errorf("missing library identifier")
	}
	if len(value) > maxSegmentLength || strings.HasPrefix(value, ".") ||
		strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, ':') {
		return fmt.Errorf("unsafe library identifier")
	}
	return nil
}

// validateFileName rejects a name that cannot safely become one flat file name.
// A leading dot is allowed (server files may legitimately start with one) but
// separators, drive syntax and the two directory aliases are not.
func validateFileName(value string) (string, error) {
	if value == "" || value == "." || value == ".." {
		return "", fmt.Errorf("unsafe file name %q", value)
	}
	if len(value) > maxFileNameLength {
		return "", fmt.Errorf("file name is too long")
	}
	if strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, ':') ||
		strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("unsafe file name %q", value)
	}
	return value, nil
}
