//go:build !windows

package fsaccess

import "os"

// Private reports whether a file or directory is inaccessible to anyone other
// than its owner.  On POSIX systems that is exactly the group/other permission
// bits being clear (0600 for files, 0700 for directories).
func Private(info os.FileInfo) bool { return info.Mode().Perm()&0077 == 0 }
