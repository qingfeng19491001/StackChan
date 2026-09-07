package meeting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisSessionStoreAtomicallyClaimsOneSessionPerMAC(t *testing.T) {
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })
	storeA := NewRedisSessionStore(clientA, "test", time.Minute)
	storeB := NewRedisSessionStore(clientB, "test", time.Minute)
	first := RedisSessionRecord{MAC: "AABBCCDDEEFF", SessionID: "session-1", State: "starting", Owner: Owner{UserID: "user-1", DeviceID: "phone-1"}}
	second := RedisSessionRecord{MAC: first.MAC, SessionID: "session-2", State: "starting", Owner: first.Owner}

	if err := storeA.Claim(context.Background(), first); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := storeB.Claim(context.Background(), second); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second claim error = %v, want %v", err, ErrSessionBusy)
	}
	loaded, err := storeB.Load(context.Background(), first.MAC)
	if err != nil || loaded == nil || loaded.SessionID != first.SessionID || loaded.Owner != first.Owner {
		t.Fatalf("loaded = %#v, error = %v", loaded, err)
	}
}

func TestRedisSessionStoreUpdatesAndReleasesOnlyMatchingSession(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	record := RedisSessionRecord{MAC: "AABBCCDDEEFF", SessionID: "session-1", State: "starting"}
	if err := store.Claim(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	record.State = "recording"
	record.NextSequence = 42
	if err := store.Save(context.Background(), record); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, _ := store.Load(context.Background(), record.MAC)
	if loaded.State != "recording" || loaded.NextSequence != 42 {
		t.Fatalf("updated record = %#v", loaded)
	}
	if released, err := store.Release(context.Background(), record.MAC, "other-session"); err != nil || released {
		t.Fatalf("foreign release = %v, %v", released, err)
	}
	if released, err := store.Release(context.Background(), record.MAC, record.SessionID); err != nil || !released {
		t.Fatalf("owner release = %v, %v", released, err)
	}
	if loaded, err := store.Load(context.Background(), record.MAC); err != nil || loaded != nil {
		t.Fatalf("record remained after release: %#v, %v", loaded, err)
	}
}

func TestManagersSharingRedisRejectConcurrentSessionsAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	managerA := NewMemoryManagerWithStore(&fakeTransport{}, store)
	managerB := NewMemoryManagerWithStore(&fakeTransport{}, store)
	first := StartCommand{Owner: Owner{UserID: "user-1", DeviceID: "phone-1"}, MAC: "AABBCCDDEEFF", SessionID: "00112233-4455-4677-8899-aabbccddeeff", CommandID: "11112233-4455-4677-8899-aabbccddeeff"}
	second := StartCommand{Owner: first.Owner, MAC: first.MAC, SessionID: "20112233-4455-4677-8899-aabbccddeeff", CommandID: "21112233-4455-4677-8899-aabbccddeeff"}

	if _, err := managerA.Start(context.Background(), first); err != nil {
		t.Fatalf("first manager start: %v", err)
	}
	if _, err := managerB.Start(context.Background(), second); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second manager start error = %v, want %v", err, ErrSessionBusy)
	}
}

func TestManagersSharingRedisDoNotResendIdempotentStartAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	transportA, transportB := &fakeTransport{}, &fakeTransport{}
	managerA := NewMemoryManagerWithStore(transportA, store)
	managerB := NewMemoryManagerWithStore(transportB, store)
	command := StartCommand{Owner: Owner{UserID: "user-1", DeviceID: "phone-1"}, MAC: "AABBCCDDEEFF", SessionID: "00112233-4455-4677-8899-aabbccddeeff", CommandID: "11112233-4455-4677-8899-aabbccddeeff"}

	first, err := managerA.Start(context.Background(), command)
	if err != nil {
		t.Fatalf("first manager start: %v", err)
	}
	second, err := managerB.Start(context.Background(), command)
	if err != nil || second != first {
		t.Fatalf("idempotent distributed start = %+v, %v; want %+v", second, err, first)
	}
	if len(transportA.controls) != 1 || len(transportB.controls) != 0 {
		t.Fatalf("start controls: manager A=%d manager B=%d", len(transportA.controls), len(transportB.controls))
	}
}

func TestManagerPersistsSequenceAndReleasesRedisAtStopBarrier(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	manager := NewMemoryManagerWithStore(&fakeTransport{}, store)
	start := StartCommand{Owner: Owner{UserID: "user-1", DeviceID: "phone-1"}, MAC: "AABBCCDDEEFF", SessionID: "00112233-4455-4677-8899-aabbccddeeff", CommandID: "11112233-4455-4677-8899-aabbccddeeff"}
	if _, err := manager.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.started", SessionID: start.SessionID, CommandID: start.CommandID, FirstSequence: uint32Ptr(0)}); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 0, 1)); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), start.MAC)
	if err != nil || loaded == nil || loaded.State != "recording" || loaded.NextSequence != 1 {
		t.Fatalf("recording state = %#v, error = %v", loaded, err)
	}
	stop := StopCommand{Owner: start.Owner, MAC: start.MAC, SessionID: start.SessionID, CommandID: "22212233-4455-4677-8899-aabbccddeeff"}
	if _, err := manager.Stop(context.Background(), stop); err != nil {
		t.Fatal(err)
	}
	last := uint32(0)
	if err := manager.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.stopped", SessionID: start.SessionID, CommandID: stop.CommandID, LastSequence: &last}); err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.Load(context.Background(), start.MAC); err != nil || loaded != nil {
		t.Fatalf("session remained in Redis after barrier: %#v, %v", loaded, err)
	}
}

func TestManagersSharingRedisHydrateDeviceNodeAcrossStopBarrier(t *testing.T) {
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })
	managerA := NewMemoryManagerWithStore(&fakeTransport{}, NewRedisSessionStore(clientA, "test", time.Minute))
	managerB := NewMemoryManagerWithStore(&fakeTransport{}, NewRedisSessionStore(clientB, "test", time.Minute))
	start := StartCommand{Owner: Owner{NodeID: "app-node", UserID: "user-1", DeviceID: "phone-1"}, MAC: "AABBCCDDEEFF", SessionID: "00112233-4455-4677-8899-aabbccddeeff", CommandID: "11112233-4455-4677-8899-aabbccddeeff"}
	if _, err := managerA.Start(context.Background(), start); err != nil {
		t.Fatalf("start on app node: %v", err)
	}
	if err := managerB.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.started", SessionID: start.SessionID, CommandID: start.CommandID, FirstSequence: uint32Ptr(0)}); err != nil {
		t.Fatalf("device node did not hydrate start state: %v", err)
	}
	stop := StopCommand{Owner: start.Owner, MAC: start.MAC, SessionID: start.SessionID, CommandID: "22212233-4455-4677-8899-aabbccddeeff"}
	if _, err := managerA.Stop(context.Background(), stop); err != nil {
		t.Fatalf("stop on app node did not refresh state: %v", err)
	}
	if err := managerB.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.stopped", SessionID: start.SessionID, CommandID: stop.CommandID}); err != nil {
		t.Fatalf("device node did not refresh stop state: %v", err)
	}
	if active := managerA.Active(start.MAC); active != nil {
		t.Fatalf("stale session remained on app node: %+v", active)
	}
}

func TestManagersSharingRedisAcceptDeviceOfferAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })
	deviceTransport, appTransport := &fakeTransport{}, &fakeTransport{}
	managerDevice := NewMemoryManagerWithStore(deviceTransport, NewRedisSessionStore(clientA, "test", time.Minute))
	managerApp := NewMemoryManagerWithStore(appTransport, NewRedisSessionStore(clientB, "test", time.Minute))
	request := DeviceStartRequest{
		MAC: "AABBCCDDEEFF", SessionID: "00112233-4455-4677-8899-aabbccddeeff",
		CommandID: "11112233-4455-4677-8899-aabbccddeeff",
	}
	if err := managerDevice.RequestStart(context.Background(), request); err != nil {
		t.Fatalf("device request: %v", err)
	}
	owner := Owner{NodeID: "app-node", MAC: request.MAC, UserID: "user-1", DeviceID: "phone-1", Generation: 7}
	result, err := managerApp.AcceptRequestedStart(context.Background(), AcceptStartCommand{
		Owner: owner, MAC: request.MAC, SessionID: request.SessionID, CommandID: request.CommandID,
	})
	if err != nil || result.State != "starting" {
		t.Fatalf("cross-instance accept = %+v, %v", result, err)
	}
	if len(appTransport.controls) != 1 || appTransport.controls[0].Message.Action != "meeting.start" {
		t.Fatalf("device controls = %#v", appTransport.controls)
	}
	loaded, err := NewRedisSessionStore(clientA, "test", time.Minute).Load(context.Background(), request.MAC)
	if err != nil || loaded == nil || loaded.State != "starting" || loaded.Owner != owner {
		t.Fatalf("promoted offer = %#v, %v", loaded, err)
	}
}

func TestDistributedDeviceOfferAllowsOnlyOneOwner(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	request := DeviceStartRequest{
		MAC: "AABBCCDDEEFF", SessionID: "00112233-4455-4677-8899-aabbccddeeff",
		CommandID: "11112233-4455-4677-8899-aabbccddeeff",
	}
	if err := NewMemoryManagerWithStore(&fakeTransport{}, store).RequestStart(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	firstTransport, secondTransport := &fakeTransport{}, &fakeTransport{}
	first := NewMemoryManagerWithStore(firstTransport, store)
	second := NewMemoryManagerWithStore(secondTransport, store)
	firstOwner := Owner{NodeID: "node-a", MAC: request.MAC, UserID: "user-1", DeviceID: "phone-1", Generation: 1}
	command := AcceptStartCommand{Owner: firstOwner, MAC: request.MAC, SessionID: request.SessionID, CommandID: request.CommandID}
	if _, err := first.AcceptRequestedStart(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	command.Owner = Owner{NodeID: "node-b", MAC: request.MAC, UserID: "user-1", DeviceID: "phone-2", Generation: 1}
	if _, err := second.AcceptRequestedStart(context.Background(), command); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("competing accept error = %v, want %v", err, ErrSessionBusy)
	}
	if len(firstTransport.controls) != 1 || len(secondTransport.controls) != 0 {
		t.Fatalf("device sends: first=%d second=%d", len(firstTransport.controls), len(secondTransport.controls))
	}
}

func TestExpiredLocalOfferCannotDeleteCrossInstancePromotion(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	now := time.Now()
	deviceTransport := &fakeTransport{}
	managerDevice := newMemoryManagerWithOptions(deviceTransport, store, func() time.Time { return now }, time.Hour, ManagerOptions{})
	request := DeviceStartRequest{
		MAC: "AABBCCDDEEFF", SessionID: "00112233-4455-4677-8899-aabbccddeeff",
		CommandID: "11112233-4455-4677-8899-aabbccddeeff",
	}
	if err := managerDevice.RequestStart(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	owner := Owner{NodeID: "node-b", MAC: request.MAC, UserID: "user-1", DeviceID: "phone-1", Generation: 2}
	managerApp := NewMemoryManagerWithStore(&fakeTransport{}, store)
	if _, err := managerApp.AcceptRequestedStart(context.Background(), AcceptStartCommand{
		Owner: owner, MAC: request.MAC, SessionID: request.SessionID, CommandID: request.CommandID,
	}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(startOfferTTL + time.Second)
	managerDevice.ExpirePending(context.Background())
	loaded, err := store.Load(context.Background(), request.MAC)
	if err != nil || loaded == nil || loaded.State != "starting" || loaded.Owner != owner {
		t.Fatalf("promoted session was removed by stale offer expiry: %#v, %v", loaded, err)
	}
	if len(deviceTransport.controls) != 1 {
		t.Fatalf("stale expiry emitted an extra control: %#v", deviceTransport.controls)
	}
}

func TestDistributedDuplicateStopDoesNotResendDeviceControl(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	owner := Owner{NodeID: "node-a", MAC: "AABBCCDDEEFF", UserID: "user-1", DeviceID: "phone-1", Generation: 3}
	start := StartCommand{
		Owner: owner, MAC: owner.MAC,
		SessionID: "00112233-4455-4677-8899-aabbccddeeff",
		CommandID: "11112233-4455-4677-8899-aabbccddeeff",
	}
	if _, err := NewMemoryManagerWithStore(&fakeTransport{}, store).Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	stop := StopCommand{
		Owner: owner, MAC: owner.MAC, SessionID: start.SessionID,
		CommandID: "22212233-4455-4677-8899-aabbccddeeff",
	}
	firstTransport, retryTransport := &fakeTransport{}, &fakeTransport{}
	if _, err := NewMemoryManagerWithStore(firstTransport, store).Stop(context.Background(), stop); err != nil {
		t.Fatal(err)
	}
	result, err := NewMemoryManagerWithStore(retryTransport, store).Stop(context.Background(), stop)
	if err != nil || result.State != "stopping" {
		t.Fatalf("duplicate stop = %+v, %v", result, err)
	}
	if len(firstTransport.controls) != 1 || len(retryTransport.controls) != 0 {
		t.Fatalf("stop controls: first=%d retry=%d", len(firstTransport.controls), len(retryTransport.controls))
	}
}

func TestManagersSharingRedisReplayAudioAcrossInstances(t *testing.T) {
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })
	storeA := NewRedisSessionStore(clientA, "test", time.Minute)
	storeB := NewRedisSessionStore(clientB, "test", time.Minute)
	deviceTransport := &fakeTransport{ownerOffline: true}
	appTransport := &fakeTransport{}
	managerDevice := NewMemoryManagerWithOptionsAndStore(deviceTransport, storeA, ManagerOptions{EnableReattach: true})
	managerApp := NewMemoryManagerWithOptionsAndStore(appTransport, storeB, ManagerOptions{EnableReattach: true})
	owner := Owner{NodeID: "node-a", MAC: "AABBCCDDEEFF", UserID: "user-1", DeviceID: "phone-1", Generation: 1}
	start := StartCommand{
		Owner: owner, MAC: owner.MAC,
		SessionID: "00112233-4455-4677-8899-aabbccddeeff",
		CommandID: "11112233-4455-4677-8899-aabbccddeeff",
	}
	if _, err := managerApp.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if err := managerDevice.OnDeviceEvent(context.Background(), start.MAC, Event{
		Action: "meeting.started", SessionID: start.SessionID,
		CommandID: start.CommandID, FirstSequence: uint32Ptr(0),
	}); err != nil {
		t.Fatal(err)
	}
	payload := encodedAudio(t, start.SessionID, 0, 7)
	if err := managerDevice.OnAudio(context.Background(), start.MAC, payload); err != nil {
		t.Fatalf("device audio: %v", err)
	}

	newOwner := owner
	newOwner.NodeID = "node-b"
	newOwner.Generation = 2
	if err := managerApp.ReattachOwner(context.Background(), ReattachCommand{
		Owner: newOwner, MAC: start.MAC, SessionID: start.SessionID,
		CommandID: "22212233-4455-4677-8899-aabbccddeeff",
	}); err != nil {
		t.Fatalf("cross-instance reattach: %v", err)
	}
	if got := audioSequences(t, appTransport.audio); len(got) != 1 || got[0] != 0 {
		t.Fatalf("replayed sequences = %v, want [0]", got)
	}
}

func TestRedisReplayIsByteBoundedAndReleasedWithSession(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	record := RedisSessionRecord{MAC: "AABBCCDDEEFF", SessionID: "session-1", State: "recording"}
	if err := store.Claim(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for sequence := uint32(0); sequence < 40; sequence++ {
		if err := store.AppendReplay(context.Background(), record.MAC, record.SessionID, RedisReplayFrame{
			Sequence: sequence, Payload: make([]byte, 4096), ReceivedAtMS: now.UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	frames, err := store.LoadReplay(context.Background(), record.MAC, record.SessionID, now.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != audioReplayMaxBytes/4096 || frames[0].Sequence != 8 || frames[len(frames)-1].Sequence != 39 {
		t.Fatalf("bounded replay = len %d, sequence %d..%d", len(frames), frames[0].Sequence, frames[len(frames)-1].Sequence)
	}
	if released, err := store.Release(context.Background(), record.MAC, record.SessionID); err != nil || !released {
		t.Fatalf("release = %v, %v", released, err)
	}
	if count := client.LLen(context.Background(), store.replayKey(record.MAC)).Val(); count != 0 {
		t.Fatalf("replay list remained after release: %d", count)
	}
}

func TestDistributedCommandsRemainIdempotentAfterCompletion(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisSessionStore(client, "test", time.Minute)
	firstTransport := &fakeTransport{}
	first := NewMemoryManagerWithStore(firstTransport, store)
	owner := Owner{NodeID: "node-a", MAC: "AABBCCDDEEFF", UserID: "user-1", DeviceID: "phone-1", Generation: 1}
	start := StartCommand{
		Owner: owner, MAC: owner.MAC,
		SessionID: "00112233-4455-4677-8899-aabbccddeeff",
		CommandID: "11112233-4455-4677-8899-aabbccddeeff",
	}
	if _, err := first.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if err := first.OnDeviceEvent(context.Background(), start.MAC, Event{
		Action: "meeting.started", SessionID: start.SessionID,
		CommandID: start.CommandID, FirstSequence: uint32Ptr(0),
	}); err != nil {
		t.Fatal(err)
	}
	stop := StopCommand{
		Owner: owner, MAC: owner.MAC, SessionID: start.SessionID,
		CommandID: "22212233-4455-4677-8899-aabbccddeeff",
	}
	if _, err := first.Stop(context.Background(), stop); err != nil {
		t.Fatal(err)
	}
	if err := first.OnDeviceEvent(context.Background(), start.MAC, Event{
		Action: "meeting.stopped", SessionID: start.SessionID, CommandID: stop.CommandID,
	}); err != nil {
		t.Fatal(err)
	}

	retryTransport := &fakeTransport{}
	retry := NewMemoryManagerWithStore(retryTransport, store)
	if result, err := retry.Start(context.Background(), start); err != nil || result.State != "starting" {
		t.Fatalf("completed start retry = %+v, %v", result, err)
	}
	if result, err := retry.Stop(context.Background(), stop); err != nil || result.State != "stopping" {
		t.Fatalf("completed stop retry = %+v, %v", result, err)
	}
	if len(retryTransport.controls) != 0 {
		t.Fatalf("completed retries resent controls: %#v", retryTransport.controls)
	}
}
