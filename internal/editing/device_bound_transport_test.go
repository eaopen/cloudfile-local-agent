package editing

import (
	"context"
	"errors"
	"testing"
)

type devicePublisherFixture struct {
	publishes int
	queries   int
	intent    Intent
}

func (f *devicePublisherFixture) PublishDeviceCommit(_ context.Context, _ Lease, intent Intent) (Receipt, error) {
	f.publishes++
	f.intent = intent
	return Receipt{IntentID: intent.ID, State: "published", Action: intent.Action,
		FileID:        "cccccccccccccccccccccccccccccccccccccccc",
		ContentDigest: intent.Digest, ExpectedFileID: intent.ExpectedFileID,
		CheckedIn: intent.Action == "checkin"}, nil
}
func (f *devicePublisherFixture) QueryDeviceCommit(_ context.Context, i Intent) (Receipt, error) {
	f.queries++
	if f.intent.ID == "" {
		return Receipt{}, errors.New("not available")
	}
	return f.PublishDeviceCommit(context.Background(), Lease{}, i)
}

func TestDeviceBoundManualUploadTransportHasNoOrdinaryFallback(t *testing.T) {
	if _, err := NewDeviceBoundTransport(nil, nil); err == nil {
		t.Fatal("nil device broker and publisher accepted")
	}
	if _, err := NewDeviceBoundTransport(&DeviceCommitBroker{}, nil); err == nil {
		t.Fatal("signed prepare without guarded publisher accepted")
	}
	publisher := &devicePublisherFixture{}
	transport, err := NewDeviceBoundTransport(&DeviceCommitBroker{}, publisher)
	if err != nil {
		t.Fatal(err)
	}
	intent := Intent{ID: "11111111-1111-4111-8111-111111111111", Action: "commit"}
	result, err := transport.Publish(context.Background(), Lease{}, intent)
	if err != nil || result.IntentID != intent.ID || publisher.publishes != 1 {
		t.Fatalf("trusted publisher not used %+v %v", result, err)
	}
}
