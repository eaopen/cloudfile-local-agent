//go:build windows

package fsaccess

import "os"

// Private reports whether a file or directory is inaccessible to anyone other
// than its owner.
//
// Windows has no POSIX permission bits: Go synthesises 0666 for files and 0777
// for directories from the read-only attribute alone, so the group/other bit
// test used on POSIX systems is true for every path here.  Applying it would
// reject every task directory, journal and session record, i.e. silently
// disable the whole local task/editing layer on Windows.  Access is really
// governed by NTFS ACLs, which os.FileInfo does not expose, so this returns
// true and the callers keep their remaining hardening (absolute path, no
// reparse point, regular file, size cap).
func Private(_ os.FileInfo) bool { return true }
