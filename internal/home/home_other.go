//go:build !windows

package home

// installRoots is empty outside Windows. The drive preference is a Windows
// deployment decision; macOS and Linux keep the per-user location.
func installRoots() []string {
	return nil
}

// FixedDrive has no meaning without Windows drive letters. Everything is a
// candidate, which mirrors how the mirror root already behaves off Windows.
func FixedDrive(string) bool {
	return true
}
