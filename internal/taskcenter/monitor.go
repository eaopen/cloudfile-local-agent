package taskcenter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"
)

// Event is a UI-only hint. The monitor never publishes, renews or releases a
// checkout. Show as a passive toast or an actionable persistent warning.
type Event struct {
	TaskID   string `json:"task_id"`
	FileName string `json:"file_name"`
	Kind     string `json:"kind"` // local-saved / needs-review
}

// Monitor polls the already-registered, trusted local copies. No OS watcher,
// browser, network or second editing workflow is required.
type Monitor struct {
	store    *Store
	quiet    time.Duration
	observed map[string]observation
}

type signature struct {
	size  int64
	mtime int64
}
type observation struct {
	current    signature
	firstSeen  time.Time
	lastDigest string
	warned     bool
}

func NewMonitor(store *Store, quiet time.Duration) (*Monitor, error) {
	if store == nil || quiet < time.Second {
		return nil, errors.New("task store and a safe quiet period required")
	}
	return &Monitor{store: store, quiet: quiet, observed: make(map[string]observation)}, nil
}

// Scan returns notices only after the file's size and mtime have been stable
// for quiet. SHA256 suppresses duplicate notices from Word/Excel auto-save and
// timestamp-only updates. Checking size/mtime after hashing avoids reporting
// a file that changed during hashing. The first scan after process restart is
// intentionally quiet until the stability delay has elapsed.
func (m *Monitor) Scan(now time.Time) ([]Event, error) {
	summaries, err := m.store.List()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(summaries))
	events := make([]Event, 0)
	for _, summary := range summaries {
		seen[summary.ID] = true
		prev := m.observed[summary.ID]
		if summary.NeedsReview {
			if !prev.warned {
				events = append(events, Event{TaskID: summary.ID, FileName: summary.FileName, Kind: "needs-review"})
			}
			prev.warned = true
			prev.firstSeen = time.Time{}
			m.observed[summary.ID] = prev
			continue
		}
		prev.warned = false
		if !summary.LocalChanged {
			// A clean local copy resets the notification episode.
			delete(m.observed, summary.ID)
			continue
		}
		t, err := m.store.Get(summary.ID)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(t.WorkingCopy)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			if !prev.warned {
				events = append(events, Event{TaskID: summary.ID, FileName: summary.FileName, Kind: "needs-review"})
			}
			prev.warned = true
			prev.firstSeen = time.Time{}
			m.observed[summary.ID] = prev
			continue
		}
		current := signature{size: info.Size(), mtime: info.ModTime().UnixNano()}
		if prev.firstSeen.IsZero() || prev.current != current {
			prev.current = current
			prev.firstSeen = now
			m.observed[summary.ID] = prev
			continue
		}
		if now.Sub(prev.firstSeen) < m.quiet {
			continue
		}
		digest, err := stableSHA256(t.WorkingCopy, current)
		if err != nil {
			// File was being saved or replaced during the hash. Never claim
			// this is a stable saved version; retry after another quiet period.
			prev.firstSeen = now
			m.observed[summary.ID] = prev
			continue
		}
		if prev.lastDigest != digest {
			events = append(events, Event{TaskID: summary.ID, FileName: summary.FileName, Kind: "local-saved"})
			prev.lastDigest = digest
		}
		m.observed[summary.ID] = prev
	}
	for id := range m.observed {
		if !seen[id] {
			delete(m.observed, id)
		}
	}
	return events, nil
}

func stableSHA256(file string, expected signature) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() ||
		before.Size() != expected.size || before.ModTime().UnixNano() != expected.mtime {
		return "", errors.New("file is changing")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	after, err := os.Lstat(file)
	if err != nil || !after.Mode().IsRegular() || after.Mode()&os.ModeSymlink != 0 ||
		after.Size() != expected.size || after.ModTime().UnixNano() != expected.mtime || !os.SameFile(before, after) {
		return "", errors.New("file changed during hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
