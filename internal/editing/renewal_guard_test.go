package editing

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func guardedLeaseFixture(t *testing.T) (FileJournal, State) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	journal := FileJournal{Path: filepath.Join(dir, "journal.json")}
	state := State{Lease: Lease{
		GuardID:    "11111111-1111-4111-8111-111111111111",
		Generation: "1", CredentialEpoch: "1",
		BaseFileID: strings.Repeat("a", 40), Token: strings.Repeat("b", 64),
	}}
	if err := journal.Save(state); err != nil {
		t.Fatal(err)
	}
	return journal, state
}

func TestRenewalGateDeduplicatesConfirmedOriginalLease(t *testing.T) {
	journal, state := guardedLeaseFixture(t)
	calls := 0
	run := func() error { calls++; return nil }
	origin := "http://10.12.1.157:8092"
	now := time.Now()
	reused, err := RenewalGate(journal, state, origin, "paired-device", "original-session", now, 45*time.Second, run)
	if err != nil || reused || calls != 1 {
		t.Fatalf("first %v %v %d", reused, err, calls)
	}
	reused, err = RenewalGate(journal, state, origin, "paired-device", "original-session", now.Add(20*time.Second), 45*time.Second, run)
	if err != nil || !reused || calls != 1 {
		t.Fatalf("recent %v %v %d", reused, err, calls)
	}
	reused, err = RenewalGate(journal, state, origin, "paired-device", "original-session", now.Add(time.Minute), 45*time.Second, run)
	if err != nil || reused || calls != 2 {
		t.Fatalf("expired cache %v %v %d", reused, err, calls)
	}
	// HTTP and HTTPS are different signed origin identities.
	reused, err = RenewalGate(journal, state, "https://10.12.1.157:8092", "paired-device", "original-session", now.Add(time.Minute), 45*time.Second, run)
	if err != nil || reused || calls != 3 {
		t.Fatalf("scheme alias %v %v %d", reused, err, calls)
	}
}

func TestRenewalGatePreservesJournalAndDeniesUnconfirmed(t *testing.T) {
	journal, state := guardedLeaseFixture(t)
	now := time.Now()
	rejected := errors.New("server did not confirm renewal")
	reused, err := RenewalGate(journal, state, "https://files.test", "device", "session", now, 45*time.Second, func() error { return rejected })
	if reused || !errors.Is(err, rejected) {
		t.Fatalf("reported unconfirmed success: %v %v", reused, err)
	}
	count := 0
	reused, err = RenewalGate(journal, state, "https://files.test", "device", "session", now, 45*time.Second, func() error { count++; return nil })
	if reused || err != nil || count != 1 {
		t.Fatalf("failed result cached: %v %v %d", reused, err, count)
	}
	before, err := journal.Load()
	if err != nil || before.Closed || before.Lease != state.Lease {
		t.Fatalf("journal changed: %+v %v", before, err)
	}
	state.Closed = true
	if err := journal.Save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := RenewalGate(journal, state, "https://files.test", "device", "session", now, 45*time.Second, func() error { return nil }); err == nil {
		t.Fatal("closed Checkout renewed")
	}
}

func TestRenewalGateBlocksConcurrentOwners(t *testing.T) {
	journal, state := guardedLeaseFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = RenewalGate(journal, state, "https://files.test", "device", "session", time.Now(), 45*time.Second,
			func() error { close(entered); <-release; return nil })
	}()
	<-entered
	executed := false
	if _, err := RenewalGate(journal, state, "https://files.test", "device", "session", time.Now(), 45*time.Second,
		func() error { executed = true; return nil }); err == nil || executed {
		t.Fatal("simultaneous renewal owners allowed")
	}
	close(release)
	wg.Wait()
	reused, err := RenewalGate(journal, state, "https://files.test", "device", "session", time.Now(), 45*time.Second, func() error { t.Fatal("redundant signed renewal"); return nil })
	if err != nil || !reused {
		t.Fatalf("recent signed confirmation unavailable %v %v", reused, err)
	}
}
