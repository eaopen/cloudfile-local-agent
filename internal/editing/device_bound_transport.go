package editing

import (
	"context"
	"errors"
)

// DeviceCommitPublisher must be a separately authenticated, current-actor,
// native-guarded CloudFile publishing transport. A read-only ticket, browser
// cookie, ordinary Seafile update-api or raw-block staging client is NOT a
// valid implementation. The server must confirm the original intent receipt.
type DeviceCommitPublisher interface {
	PublishDeviceCommit(context.Context, Lease, Intent) (Receipt, error)
	QueryDeviceCommit(context.Context, Intent) (Receipt, error)
}

// DeviceBoundTransport is the ManualSubmitter/Workflow boundary that uses a
// real device-signed Broker Prepare before publication. Fails closed unless
// both real stages exist. There is no fallback to SessionHTTPTransport or
// ordinary uploads, and this package does not provide a production publisher.
type DeviceBoundTransport struct {
	broker    *DeviceCommitBroker
	publisher DeviceCommitPublisher
}

func NewDeviceBoundTransport(broker *DeviceCommitBroker, publisher DeviceCommitPublisher) (*DeviceBoundTransport, error) {
	if broker == nil || publisher == nil {
		return nil, errors.New("current device broker and native conditional publisher required")
	}
	return &DeviceBoundTransport{broker: broker, publisher: publisher}, nil
}
func (t *DeviceBoundTransport) Prepare(ctx context.Context, lease Lease, intent Intent) error {
	return t.broker.PrepareIntent(ctx, lease, intent)
}
func (t *DeviceBoundTransport) Publish(ctx context.Context, lease Lease, intent Intent) (Receipt, error) {
	return t.publisher.PublishDeviceCommit(ctx, lease, intent)
}
func (t *DeviceBoundTransport) Query(ctx context.Context, intent Intent) (Receipt, error) {
	return t.publisher.QueryDeviceCommit(ctx, intent)
}

var _ Transport = (*DeviceBoundTransport)(nil)
