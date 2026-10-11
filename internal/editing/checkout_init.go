package editing

import (
	"errors"
	"os"
	"path/filepath"
)

// InitializePrivateCheckout is the only transition from a device-verified
// Checkout grant to a durable local Agent2 edit session. No browser message
// may directly provide a token, arbitrary journal pathname or local filename.
// Unknown/replaced or pending journals are never overwritten.
func InitializePrivateCheckout(journal FileJournal, lease Lease) (bool, error) {
	if !ValidCheckoutLease(lease) {
		return false, errors.New("current bounded Checkout lease required")
	}
	dir := filepath.Dir(journal.Path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("private edit session directory required")
	}
	activity, err := holdWindowsActivity()
	if err != nil {
		return false, err
	}
	if activity != nil {
		defer activity.Close()
	}
	lock, err := lockSession(filepath.Join(dir, ".operation.lock"))
	if err != nil {
		return false, err
	}
	defer lock.Close()
	existing, err := journal.Load()
	if err == nil {
		if existing.Closed || existing.Lease != lease {
			return false, errors.New("existing Checkout journal differs; manual reconciliation required")
		}
		return true, nil
	}
	if !os.IsNotExist(err) {
		return false, errors.New("invalid existing journal must not be overwritten")
	}
	state := State{SchemaVersion: 1, Lease: lease}
	if err = journal.Save(state); err != nil {
		return false, err
	}
	return false, nil
}
