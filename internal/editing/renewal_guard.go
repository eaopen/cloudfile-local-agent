package editing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/eaopen/cloudfile-local-agent/internal/fsaccess"
)

const maxRenewalRecord = 1024

// RenewalGate coordinates foreground v0.7 controllers and the v0.8 resident
// monitor using the SAME original Checkout journal directory. The local
// index never becomes the lease authority. The first caller executes the
// signed renewal; later callers reuse a RECENT confirmed result for this exact
// generation and paired identity. No cloud write or lock release occurs here.
func RenewalGate(journal FileJournal, state State, instance, deviceID, sessionID string,
	now time.Time, quiet time.Duration, send func() error) (reused bool, err error) {
	if send == nil || now.IsZero() || quiet <= 0 || quiet > 4*time.Minute ||
		state.Closed || !ValidCheckoutLease(state.Lease) ||
		instance == "" || deviceID == "" || sessionID == "" {
		return false, errors.New("current guarded renewal required")
	}
	dir := filepath.Dir(journal.Path)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !fsaccess.Private(info) {
		return false, errors.New("private Checkout directory required")
	}
	// Cross-process OS lock. Existing editing lock implementation uses flock
	// on macOS and exclusive Win32 CreateFile sharing on Windows.
	hold, err := lockSession(filepath.Join(dir, "renew-owner.lock"))
	if err != nil {
		return false, errors.New("Checkout renewal is owned by another process")
	}
	defer hold.Close()
	// The Journal may have changed (Checkin/Abandon or another commit) since
	// the caller's first read, so refresh it WHILE holding the lock.
	current, err := journal.Load()
	if err != nil || current.Closed || !ValidCheckoutLease(current.Lease) ||
		current.Lease != state.Lease {
		return false, errors.New("original Checkout lease changed; refresh status")
	}
	keyMaterial := instance + "\x00" + deviceID + "\x00" + sessionID + "\x00" +
		current.Lease.GuardID + "\x00" + current.Lease.Generation + "\x00" +
		current.Lease.CredentialEpoch + "\x00" + current.Lease.BaseFileID
	sum := sha256.Sum256([]byte(keyMaterial))
	expected := hex.EncodeToString(sum[:])
	recordPath := filepath.Join(dir, "renew-confirmed.json")
	var previous struct {
		Key string `json:"key"`
		At  int64  `json:"at"`
	}
	if fileInfo, statErr := os.Lstat(recordPath); statErr == nil {
		if !fileInfo.Mode().IsRegular() || !fsaccess.Private(fileInfo) ||
			fileInfo.Size() <= 0 || fileInfo.Size() > maxRenewalRecord {
			return false, errors.New("renewal confirmation requires review")
		}
		data, readErr := os.ReadFile(recordPath)
		if readErr != nil || json.Unmarshal(data, &previous) != nil {
			return false, errors.New("renewal confirmation requires review")
		}
		if previous.Key == expected && previous.At > 0 &&
			now.Unix() >= previous.At && now.Sub(time.Unix(previous.At, 0)) < quiet {
			return true, nil
		}
	} else if !os.IsNotExist(statErr) {
		return false, errors.New("renewal confirmation unavailable")
	}
	if err := send(); err != nil {
		return false, err
	}
	// A confirmed server response is the only event allowed to update the
	// local dedup record. Crash between response/write causes another real
	// signed renew, which is safe and never a false success.
	encoded, err := json.Marshal(map[string]any{"key": expected, "at": time.Now().Unix()})
	if err != nil {
		return false, err
	}
	temp, err := os.CreateTemp(dir, ".renew-ack-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(encoded)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil {
		return false, err
	}
	if closeErr != nil {
		return false, closeErr
	}
	if err = os.Rename(temp.Name(), recordPath); err != nil {
		return false, err
	}
	return false, nil
}
