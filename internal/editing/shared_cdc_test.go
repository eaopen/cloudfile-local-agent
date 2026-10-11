package editing

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eaopen/cloudfile-local-agent/internal/blockdelta"
)

// The SAME CDC engine is called from optimistic and checkout adapters.
// This checks byte correctness, no duplicate block stage, selection economics,
// and no silent repeat of a source-changing commit intent.
func TestSharedCDCPlanStagesOnlyRealMissingBlocks(t *testing.T) {
	t.Setenv("CLOUDFILE_AGENT2_CDC_ENABLED", "1")
	large := append(append(bytes.Repeat([]byte("A"), 7<<20),
		bytes.Repeat([]byte("B"), 8<<20)...), bytes.Repeat([]byte("C"), 5<<20)...)
	source := filepath.Join(t.TempDir(), "cad.dwg")
	if err := os.WriteFile(source, large, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := CaptureSnapshot(source, t.TempDir(), 128<<20)
	if err != nil {
		t.Fatal(err)
	}
	intent := Intent{ID: "11111111-1111-4111-8111-111111111111",
		ExpectedFileID: strings.Repeat("a", 40), Digest: snapshot.Digest,
		Snapshot: snapshot.Path, SnapshotSize: snapshot.Size}
	manifest, err := blockdelta.ScanSnapshot(snapshot.Path, 1)
	if err != nil || len(manifest.Blocks) < 2 {
		t.Fatalf("missing CDC blocks %+v %v", manifest, err)
	}
	missing := manifest.Blocks[len(manifest.Blocks)-1]
	if !PickCDC(intent.SnapshotSize, missing.Size) {
		t.Fatalf("small missing block should justify delta: %d/%d", missing.Size, intent.SnapshotSize)
	}
	grants, stages, publishes := 0, 0, 0
	grantFn := func(context.Context) (string, error) {
		grants++
		return "short-live-device-grant", nil
	}
	postFn := func(_ context.Context, operation, grant string, item Intent, reader io.Reader,
		size int64, contentType, blockID string) ([]byte, error) {
		if grant != "short-live-device-grant" || item.ID != intent.ID {
			t.Fatal("foreign device or Intent in shared CDC")
		}
		raw, _ := io.ReadAll(reader)
		switch operation {
		case "plan":
			var planned struct {
				Blocks []struct {
					ID string `json:"id"`
				} `json:"blocks"`
			}
			if json.Unmarshal(raw, &planned) != nil || len(planned.Blocks) != len(manifest.Blocks) {
				t.Fatalf("invalid ordered CDC plan")
			}
			return json.Marshal(map[string]any{"file_id": manifest.FileID,
				"snapshot_size": manifest.Size, "intent_id": intent.ID,
				"missing": []string{missing.ID}})
		case "stage":
			stages++
			if blockID != missing.ID || size != missing.Size ||
				contentType != "application/octet-stream" ||
				!bytes.Equal(raw, large[len(large)-int(missing.Size):]) {
				t.Fatal("CDC stage did not carry the precise missing bytes")
			}
			return json.Marshal(map[string]any{"staged": true, "block_id": blockID,
				"intent_id": intent.ID})
		case "publish":
			publishes++
			if size != 0 || len(raw) != 0 {
				t.Fatal("CDC final publication carried unapproved extra bytes")
			}
			return []byte("{}"), nil
		default:
			t.Fatalf("unsupported shared CDC operation %s", operation)
		}
		return nil, nil
	}
	final, fileID, used, err := executeSharedCDC(context.Background(), intent, grantFn, postFn)
	if err != nil || !used || fileID != manifest.FileID || string(final) != "{}" {
		t.Fatalf("shared CDC failed %s %t %v", fileID, used, err)
	}
	if stages != 1 || publishes != 1 || grants < 3 {
		t.Fatalf("bad sparse stage counts: %d %d %d", stages, publishes, grants)
	}
}
