package web_socket

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/redis/go-redis/v9"
	"stackChan/internal/meeting"
	"stackChan/internal/model"
	wsprotocol "stackChan/internal/web_socket/protocol"
)

func TestMeetingTransportPublishesRemoteDeviceDeliveryWhenNotLocal(t *testing.T) {
	fake := &testMeetingCluster{nodeID: "node-a"}
	withMeetingCluster(t, fake)
	message := meeting.ControlMessage{Action: "meeting.start", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if err := (wsMeetingTransport{}).SendDevice(context.Background(), "AABBCCDDEEFF", message); err != nil {
		t.Fatal(err)
	}
	if fake.delivery.Kind != clusterToDevice || fake.delivery.MAC != "AABBCCDDEEFF" || fake.delivery.Control == nil || fake.delivery.Control.Action != "meeting.start" {
		t.Fatalf("published delivery = %#v", fake.delivery)
	}
}

func TestDisconnectedLocalDeviceOverridesStaleClusterPresence(t *testing.T) {
	mac := "AA2334455667"
	client := model.NewStackChanClient(mac, nil, nil, nil, false)
	defer client.CloseWriterCoroutine()
	stackChanClientPool.Store(mac, client)
	defer stackChanClientPool.Delete(mac)

	// Redis may still carry presence for a previous socket generation. Once
	// this process has observed its local device socket close, REST/ticket
	// callers must see the device as offline.
	withMeetingCluster(t, &testMeetingCluster{nodeID: "node-a"})
	if generation, online := DeviceConnectionGeneration(mac); online || generation != 0 {
		t.Fatalf("disconnected local device reported online: generation=%d online=%v", generation, online)
	}
}

func TestClusterOwnerRouteRequiresOwningNode(t *testing.T) {
	server, peer := websocketPair(t)
	defer peer.Close()
	client := model.NewAppClient("AABBCCDDEEFF", server, "phone")
	defer client.CloseWriterCoroutine()
	client.SetUserID("owner")
	client.SetMeetingAuthorization("owner", "phone", client.ConnectionGeneration())
	client.SelectMeetingV1(client.ConnectionGeneration())
	addAppClient(client)
	defer appClientPool.Delete(client.GetMac())

	withMeetingCluster(t, &testMeetingCluster{nodeID: "node-a"})
	owner := meeting.Owner{NodeID: "node-b", MAC: client.GetMac(), UserID: "owner", DeviceID: "phone", Generation: client.ConnectionGeneration()}
	if found := findOwnerClient(owner); found != nil {
		t.Fatal("foreign node found a local owner connection")
	}
	owner.NodeID = "node-a"
	control := meeting.ControlMessage{Action: "meeting.error", SessionID: uuid.NewString(), CommandID: uuid.NewString(), Code: "DEVICE_ERROR"}
	deliverClusterMessage(context.Background(), clusterDelivery{Kind: clusterToOwner, Owner: owner, Control: &control})
	frame := readBinaryMessage(t, peer)
	kind, payload, err := wsprotocol.ParseBinaryMessage(frame)
	if err != nil || kind != MeetingControl {
		t.Fatalf("delivered control kind=%d err=%v", kind, err)
	}
	parsed, err := wsprotocol.ParseMeetingControl(payload)
	if err != nil || parsed.Action != "meeting.error" || parsed.Code != "DEVICE_ERROR" {
		t.Fatalf("delivered control=%+v err=%v", parsed, err)
	}
}

type testMeetingCluster struct {
	nodeID   string
	delivery clusterDelivery
}

func (t *testMeetingCluster) NodeID() string { return t.nodeID }
func (t *testMeetingCluster) Publish(_ context.Context, delivery clusterDelivery) error {
	t.delivery = delivery
	return nil
}
func (t *testMeetingCluster) RegisterDevice(context.Context, string) (uint64, error) {
	return 1, nil
}
func (t *testMeetingCluster) TouchDevice(context.Context, string, uint64) error { return nil }
func (t *testMeetingCluster) UnregisterDevice(context.Context, string, uint64) error {
	return nil
}
func (t *testMeetingCluster) DeviceGeneration(context.Context, string) (uint64, bool) {
	return 1, true
}
func (t *testMeetingCluster) Close() error { return nil }

func TestRedisDevicePresenceUsesGlobalGeneration(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	cluster := &redisMeetingCluster{client: client, channel: "test:meeting:deliveries", nodeID: "node-a"}
	ctx := context.Background()
	first, err := cluster.RegisterDevice(ctx, "AABBCCDDEEFF")
	if err != nil {
		t.Fatal(err)
	}
	second, err := cluster.RegisterDevice(ctx, "AABBCCDDEEFF")
	if err != nil || second <= first {
		t.Fatalf("generations first=%d second=%d error=%v", first, second, err)
	}
	if err := cluster.UnregisterDevice(ctx, "AABBCCDDEEFF", first); err != nil {
		t.Fatal(err)
	}
	if got, online := cluster.DeviceGeneration(ctx, "AABBCCDDEEFF"); !online || got != second {
		t.Fatalf("stale unregister removed current presence: %d, %v", got, online)
	}
	server.FastForward(devicePresenceTTL - time.Second)
	if err := cluster.TouchDevice(ctx, "AABBCCDDEEFF", second); err != nil {
		t.Fatal(err)
	}
	server.FastForward(2 * time.Second)
	if _, online := cluster.DeviceGeneration(ctx, "AABBCCDDEEFF"); !online {
		t.Fatal("touch did not renew device presence")
	}
	if err := cluster.UnregisterDevice(ctx, "AABBCCDDEEFF", second); err != nil {
		t.Fatal(err)
	}
	if _, online := cluster.DeviceGeneration(ctx, "AABBCCDDEEFF"); online {
		t.Fatal("current unregister left device online")
	}
}

func withMeetingCluster(t *testing.T, cluster MeetingCluster) {
	t.Helper()
	meetingClusterMu.Lock()
	previous := meetingCluster
	meetingCluster = cluster
	meetingClusterMu.Unlock()
	t.Cleanup(func() {
		meetingClusterMu.Lock()
		meetingCluster = previous
		meetingClusterMu.Unlock()
	})
}

var _ = websocket.BinaryMessage
