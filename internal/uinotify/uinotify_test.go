package uinotify

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateConfigDir points os.UserConfigDir() at a temp directory on every
// platform (Windows reads APPDATA, Unix reads XDG_CONFIG_HOME/HOME).
func isolateConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("APPDATA", dir)
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "Local"))
	return dir
}

func TestPostAndReadRoundTrip(t *testing.T) {
	isolateConfigDir(t)
	if events, next, err := Read(0); err != nil || len(events) != 0 || next != 0 {
		t.Fatalf("an absent spool must read as empty: %v %v %v", events, next, err)
	}
	if err := Post(Event{Kind: KindOpened, Mode: "local-view", File: "图纸.docx", Dir: `D:\x`}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := Post(Event{Kind: KindOpened, Mode: "local-edit", File: "b.dwg"}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	events, next, err := Read(0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("want 2 events, got %d", len(events))
	}
	if events[0].File != "图纸.docx" || events[0].Mode != "local-view" || events[0].At == 0 {
		t.Fatalf("first event lost data: %+v", events[0])
	}
	if events[1].Kind != KindOpened || events[1].Dir != "" {
		t.Fatalf("second event lost data: %+v", events[1])
	}

	// 从尾偏移再读：没有新事件，也不该重复投递。
	again, next2, err := Read(next)
	if err != nil || len(again) != 0 || next2 != next {
		t.Fatalf("second read must be empty and stable: %v %v %v", again, next2, err)
	}
}

func TestReadWaitsForAWholeLine(t *testing.T) {
	isolateConfigDir(t)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// 半行：写入方还在写，读方必须原地等，不能把半个 JSON 当事件。
	if err := os.WriteFile(path, []byte(`{"at":1,"kind":"opened"`), 0600); err != nil {
		t.Fatal(err)
	}
	events, next, err := Read(0)
	if err != nil || len(events) != 0 || next != 0 {
		t.Fatalf("a partial line must not be consumed: %v %v %v", events, next, err)
	}
}

func TestReadSkipsBrokenLinesAndRecoversFromTruncation(t *testing.T) {
	isolateConfigDir(t)
	path, _ := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body := "{ not json }\n" + `{"at":7,"kind":"opened","file":"ok.docx"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	events, _, err := Read(0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 || events[0].File != "ok.docx" {
		t.Fatalf("a broken line must be skipped, not fatal: %+v", events)
	}

	// 偏移超过文件长度（被压缩过）时必须回到文件头，而不是报错或空转。
	events, _, err = Read(1 << 20)
	if err != nil {
		t.Fatalf("Read past EOF: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("a stale offset must re-read from the start: %+v", events)
	}
}

func TestPostCompactsInsteadOfGrowingForever(t *testing.T) {
	isolateConfigDir(t)
	pad := make([]byte, 512)
	for index := range pad {
		pad[index] = 'x'
	}
	for round := 0; round < 400; round++ {
		if err := Post(Event{Kind: KindOpened, File: string(pad)}); err != nil {
			t.Fatalf("Post %d: %v", round, err)
		}
	}
	path, _ := Path()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxBytes {
		t.Fatalf("spool must stay under %d bytes, got %d", maxBytes, info.Size())
	}
	// 压缩之后仍然必须能读到合法事件（可能只剩后半段）。
	events, _, err := Read(0)
	if err != nil {
		t.Fatalf("Read after compaction: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("compaction must keep the newest events")
	}
	for _, event := range events {
		if event.Kind != KindOpened {
			t.Fatalf("compaction produced a broken event: %+v", event)
		}
	}
}

func TestClaimStartupNoticeFiresOnce(t *testing.T) {
	isolateConfigDir(t)
	first, err := ClaimStartupNotice()
	if err != nil || !first {
		t.Fatalf("first claim must be true: %v %v", first, err)
	}
	second, err := ClaimStartupNotice()
	if err != nil || second {
		t.Fatalf("second claim must be false: %v %v", second, err)
	}
}
