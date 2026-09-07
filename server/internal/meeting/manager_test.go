package meeting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	wsprotocol "stackChan/internal/web_socket/protocol"
)

func TestDeviceStartRequestOffersBeforeFirstOwnerAccepts(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManagerWithOptions(transport, ManagerOptions{EnableReattach: true})
	request := DeviceStartRequest{MAC: "AA:BB:CC:DD:EE:FF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if err := manager.RequestStart(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(transport.controls) != 1 || transport.controls[0].Message.Action != "meeting.start-offered" || transport.controls[0].MAC != "AABBCCDDEEFF" {
		t.Fatalf("offer routes = %+v", transport.controls)
	}
	if manager.Active(request.MAC) != nil {
		t.Fatal("device started before an Auro owner accepted")
	}

	owner := Owner{UserID: "u1", DeviceID: "phone1"}
	result, err := manager.AcceptRequestedStart(context.Background(), AcceptStartCommand{Owner: owner, MAC: request.MAC, SessionID: request.SessionID, CommandID: request.CommandID})
	if err != nil || result.State != "starting" {
		t.Fatalf("accept result=%+v err=%v", result, err)
	}
	if len(transport.controls) != 2 || transport.controls[1].Message.Action != "meeting.start" {
		t.Fatalf("device controls after accept = %+v", transport.controls)
	}
	if active := manager.Active(request.MAC); active == nil || active.Owner != owner {
		t.Fatalf("active session = %+v", active)
	}
	_, err = manager.AcceptRequestedStart(context.Background(), AcceptStartCommand{Owner: Owner{UserID: "u2", DeviceID: "phone2"}, MAC: request.MAC, SessionID: request.SessionID, CommandID: request.CommandID})
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second accept = %v", err)
	}
}

func TestDeviceStartOfferExpiresWithoutStartingCodec(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	transport := &fakeTransport{}
	manager := newMemoryManagerWithOptions(transport, nil, func() time.Time { return now }, time.Hour, ManagerOptions{EnableReattach: true})
	request := DeviceStartRequest{MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if err := manager.RequestStart(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	manager.ExpirePending(context.Background())
	if manager.Active(request.MAC) != nil {
		t.Fatal("expired offer became active")
	}
	last := transport.controls[len(transport.controls)-1]
	if last.Message.Action != "meeting.error" || last.Message.Code != "START_TIMEOUT" {
		t.Fatalf("timeout route = %+v", last)
	}
	_, err := manager.AcceptRequestedStart(context.Background(), AcceptStartCommand{Owner: Owner{UserID: "u1", DeviceID: "phone1"}, MAC: request.MAC, SessionID: request.SessionID, CommandID: request.CommandID})
	if !errors.Is(err, ErrStartTimeout) {
		t.Fatalf("expired accept = %v", err)
	}
}

func TestDeviceStopRequestNotifiesStableOwnerAndUsesSameBarrier(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	start := StartCommand{Owner: Owner{MAC: "AABBCCDDEEFF", UserID: "u1", DeviceID: "phone1", Generation: 1}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if _, err := manager.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.started", SessionID: start.SessionID, CommandID: start.CommandID, FirstSequence: uint32Ptr(0)}); err != nil {
		t.Fatal(err)
	}
	stopCommandID := uuid.NewString()
	if err := manager.RequestStop(context.Background(), DeviceStopRequest{MAC: start.MAC, SessionID: start.SessionID, CommandID: stopCommandID, Reason: "user"}); err != nil {
		t.Fatal(err)
	}
	last := transport.controls[len(transport.controls)-1]
	if last.Owner != start.Owner || last.Message.Action != "meeting.stop" || last.Message.CommandID != stopCommandID {
		t.Fatalf("owner stop route = %+v", last)
	}
	if err := manager.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.stopped", SessionID: start.SessionID, CommandID: stopCommandID}); err != nil {
		t.Fatal(err)
	}
	if manager.Active(start.MAC) != nil {
		t.Fatal("device-stopped barrier left session active")
	}
}

func TestReattachReplaysOnlyFramesAfterLastReceivedSequence(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	transport := &fakeTransport{}
	manager := newMemoryManagerWithOptions(transport, nil, func() time.Time { return now }, time.Hour, ManagerOptions{EnableReattach: true})
	start := StartCommand{Owner: Owner{MAC: "AABBCCDDEEFF", UserID: "u1", DeviceID: "phone1", Generation: 1}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	_, _ = manager.Start(context.Background(), start)
	_ = manager.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.started", SessionID: start.SessionID, CommandID: start.CommandID, FirstSequence: uint32Ptr(0)})
	if err := manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 0, 1)); err != nil {
		t.Fatal(err)
	}
	transport.ownerOffline = true
	if err := manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 2, 3)); err != nil {
		t.Fatal(err)
	}
	transport.ownerOffline = false
	lastReceived := uint32(0)
	reattachedOwner := start.Owner
	reattachedOwner.Generation++
	if err := manager.ReattachOwner(context.Background(), ReattachCommand{Owner: reattachedOwner, MAC: start.MAC, SessionID: start.SessionID, CommandID: uuid.NewString(), LastReceivedSequence: &lastReceived}); err != nil {
		t.Fatal(err)
	}
	if got := audioSequences(t, transport.audio); len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Fatalf("delivered sequences = %v", got)
	}
}

func TestReattachReplayHandlesUint32SequenceRollover(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	transport := &fakeTransport{ownerOffline: true}
	manager := newMemoryManagerWithOptions(transport, nil, func() time.Time { return now }, time.Hour, ManagerOptions{EnableReattach: true})
	start := StartCommand{Owner: Owner{MAC: "AABBCCDDEEFF", UserID: "u1", DeviceID: "phone1", Generation: 1}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if _, err := manager.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.started", SessionID: start.SessionID, CommandID: start.CommandID, FirstSequence: uint32Ptr(0)}); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.activeByMAC[start.MAC].NextSequence = 0xffffffff
	manager.mu.Unlock()
	if err := manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 0xffffffff, 1)); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 0, 2)); err != nil {
		t.Fatal(err)
	}

	transport.ownerOffline = false
	lastReceived := uint32(0xffffffff)
	newOwner := start.Owner
	newOwner.Generation++
	if err := manager.ReattachOwner(context.Background(), ReattachCommand{
		Owner: newOwner, MAC: start.MAC, SessionID: start.SessionID,
		CommandID: uuid.NewString(), LastReceivedSequence: &lastReceived,
	}); err != nil {
		t.Fatal(err)
	}
	if got := audioSequences(t, transport.audio); len(got) != 1 || got[0] != 0 {
		t.Fatalf("rollover replay sequences = %v, want [0]", got)
	}
}

func TestReattachOutsideFiveSecondWindowReturnsAudioGap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	transport := &fakeTransport{}
	manager := newMemoryManagerWithOptions(transport, nil, func() time.Time { return now }, time.Hour, ManagerOptions{EnableReattach: true})
	start := StartCommand{Owner: Owner{MAC: "AABBCCDDEEFF", UserID: "u1", DeviceID: "phone1", Generation: 1}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	_, _ = manager.Start(context.Background(), start)
	_ = manager.OnDeviceEvent(context.Background(), start.MAC, Event{Action: "meeting.started", SessionID: start.SessionID, CommandID: start.CommandID, FirstSequence: uint32Ptr(0)})
	transport.ownerOffline = true
	_ = manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 0, 1))
	_ = manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 1, 2))
	now = now.Add(6 * time.Second)
	_ = manager.OnAudio(context.Background(), start.MAC, encodedAudio(t, start.SessionID, 2, 3))
	transport.ownerOffline = false
	lastReceived := uint32(0)
	reattachedOwner := start.Owner
	reattachedOwner.Generation++
	err := manager.ReattachOwner(context.Background(), ReattachCommand{Owner: reattachedOwner, MAC: start.MAC, SessionID: start.SessionID, CommandID: uuid.NewString(), LastReceivedSequence: &lastReceived})
	if !errors.Is(err, ErrAudioGap) {
		t.Fatalf("reattach outside window = %v", err)
	}
}

func TestStartIsIdempotentAndOneSessionPerMAC(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	command := StartCommand{Owner: Owner{UserID: "u1", DeviceID: "p1"}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	first, err := manager.Start(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Start(context.Background(), command)
	if err != nil || first != second {
		t.Fatalf("idempotent start: first=%+v second=%+v err=%v", first, second, err)
	}
	if len(transport.controls) != 1 {
		t.Fatalf("sent %d start controls", len(transport.controls))
	}
	busy := command
	busy.SessionID, busy.CommandID = uuid.NewString(), uuid.NewString()
	if _, err := manager.Start(context.Background(), busy); !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("second session = %v", err)
	}
}

func TestDeviceDisconnectAbortsActiveSessionAndNotifiesOwner(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	start := StartCommand{Owner: Owner{UserID: "u1", DeviceID: "phone1"}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if _, err := manager.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}

	manager.OnDeviceDisconnect(context.Background(), start.MAC)

	if manager.Active(start.MAC) != nil {
		t.Fatal("device disconnect left the meeting active")
	}
	last := transport.controls[len(transport.controls)-1]
	if last.Owner != start.Owner || last.Message.Action != "meeting.error" || last.Message.Code != "DEVICE_OFFLINE" || last.Message.SessionID != start.SessionID {
		t.Fatalf("disconnect notification = %+v", last)
	}
}

func TestDeviceDisconnectCancelsPendingOffer(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	request := DeviceStartRequest{MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if err := manager.RequestStart(context.Background(), request); err != nil {
		t.Fatal(err)
	}

	manager.OnDeviceDisconnect(context.Background(), request.MAC)

	_, err := manager.AcceptRequestedStart(context.Background(), AcceptStartCommand{Owner: Owner{UserID: "u1", DeviceID: "phone1"}, MAC: request.MAC, SessionID: request.SessionID, CommandID: request.CommandID})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("accept after device disconnect = %v", err)
	}
	last := transport.controls[len(transport.controls)-1]
	if last.MAC != request.MAC || last.Message.Action != "meeting.error" || last.Message.Code != "DEVICE_OFFLINE" {
		t.Fatalf("pending disconnect notification = %+v", last)
	}
}

func TestAudioRoutesOnlyToStableOwnerAndRequiresSequence(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	command := StartCommand{Owner: Owner{UserID: "u1", DeviceID: "p1"}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	_, _ = manager.Start(context.Background(), command)
	if err := manager.OnDeviceEvent(context.Background(), command.MAC, Event{Action: "meeting.started", SessionID: command.SessionID, CommandID: command.CommandID, FirstSequence: uint32Ptr(0)}); err != nil {
		t.Fatal(err)
	}
	payload, _ := wsprotocol.EncodeMeetingAudio(wsprotocol.MeetingAudioEnvelope{Version: 1, SessionID: uuid.MustParse(command.SessionID), Sequence: 0, CaptureTimestampMS: 1, Opus: []byte{1}})
	if err := manager.OnAudio(context.Background(), command.MAC, payload); err != nil {
		t.Fatal(err)
	}
	if len(transport.audio) != 1 || transport.audio[0].owner != command.Owner {
		t.Fatalf("audio routes = %+v", transport.audio)
	}

	bad, _ := wsprotocol.EncodeMeetingAudio(wsprotocol.MeetingAudioEnvelope{Version: 1, SessionID: uuid.MustParse(command.SessionID), Sequence: 2, CaptureTimestampMS: 2, Opus: []byte{2}})
	if err := manager.OnAudio(context.Background(), command.MAC, bad); !errors.Is(err, ErrAudioGap) {
		t.Fatalf("sequence gap = %v", err)
	}
}

func TestStopRequiresOwnerAndCompletesOnDeviceBarrier(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	command := StartCommand{Owner: Owner{UserID: "u1", DeviceID: "p1"}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	_, _ = manager.Start(context.Background(), command)
	_ = manager.OnDeviceEvent(context.Background(), command.MAC, Event{Action: "meeting.started", SessionID: command.SessionID, CommandID: command.CommandID, FirstSequence: uint32Ptr(0)})
	payload, _ := wsprotocol.EncodeMeetingAudio(wsprotocol.MeetingAudioEnvelope{Version: 1, SessionID: uuid.MustParse(command.SessionID), Sequence: 0, CaptureTimestampMS: 1, Opus: []byte{1}})
	if err := manager.OnAudio(context.Background(), command.MAC, payload); err != nil {
		t.Fatal(err)
	}
	stop := StopCommand{Owner: Owner{UserID: "u2", DeviceID: "p2"}, MAC: command.MAC, SessionID: command.SessionID, CommandID: uuid.NewString()}
	if _, err := manager.Stop(context.Background(), stop); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("foreign stop = %v", err)
	}
	stop.Owner = command.Owner
	if _, err := manager.Stop(context.Background(), stop); err != nil {
		t.Fatal(err)
	}
	last := uint32(0)
	if err := manager.OnDeviceEvent(context.Background(), command.MAC, Event{Action: "meeting.stopped", SessionID: command.SessionID, CommandID: stop.CommandID, LastSequence: &last}); err != nil {
		t.Fatal(err)
	}
	if manager.Active(command.MAC) != nil {
		t.Fatal("session remained active after stopped barrier")
	}
}

func TestOwnerDisconnectAndSendFailureInterruptOnlyExactAttachment(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	owner := Owner{MAC: "AABBCCDDEEFF", UserID: "u1", DeviceID: "phone", Generation: 7}
	start := StartCommand{Owner: owner, MAC: owner.MAC, SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if _, err := manager.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	manager.OnOwnerDisconnect(context.Background(), Owner{MAC: owner.MAC, UserID: owner.UserID, DeviceID: owner.DeviceID, Generation: 6})
	if manager.Active(owner.MAC) == nil {
		t.Fatal("old generation disconnected the replacement owner")
	}
	manager.OnOwnerDisconnect(context.Background(), owner)
	if manager.Active(owner.MAC) != nil {
		t.Fatal("current owner disconnect left meeting active")
	}
	last := transport.controls[len(transport.controls)-1]
	if last.MAC != owner.MAC || last.Message.Action != "meeting.stop" || last.Message.Reason != "interrupted" {
		t.Fatalf("device interruption = %+v", last)
	}

	transport.failDevice = true
	start = StartCommand{Owner: owner, MAC: owner.MAC, SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if _, err := manager.Start(context.Background(), start); err == nil || manager.Active(owner.MAC) != nil {
		t.Fatalf("device send failure err=%v active=%+v", err, manager.Active(owner.MAC))
	}
}

func TestPhase2RejectsReattachAndPhase3RequiresNewAuthorizedGeneration(t *testing.T) {
	owner := Owner{MAC: "AABBCCDDEEFF", UserID: "u1", DeviceID: "phone", Generation: 1}
	start := StartCommand{Owner: owner, MAC: owner.MAC, SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	phase2 := NewMemoryManager(&fakeTransport{})
	_, _ = phase2.Start(context.Background(), start)
	if err := phase2.ReattachOwner(context.Background(), ReattachCommand{Owner: owner, MAC: owner.MAC, SessionID: start.SessionID, CommandID: uuid.NewString()}); !errors.Is(err, ErrReattachDisabled) {
		t.Fatalf("Phase 2 reattach = %v", err)
	}

	phase3 := NewMemoryManagerWithOptions(&fakeTransport{}, ManagerOptions{EnableReattach: true})
	_, _ = phase3.Start(context.Background(), start)
	old := ReattachCommand{Owner: owner, MAC: owner.MAC, SessionID: start.SessionID, CommandID: uuid.NewString()}
	if err := phase3.ReattachOwner(context.Background(), old); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("same generation reattach = %v", err)
	}
	newOwner := owner
	newOwner.Generation = 2
	if err := phase3.ReattachOwner(context.Background(), ReattachCommand{Owner: newOwner, MAC: owner.MAC, SessionID: start.SessionID, CommandID: uuid.NewString()}); err != nil {
		t.Fatalf("authorized new attachment reattach = %v", err)
	}
	if got := phase3.Active(owner.MAC); got == nil || got.Owner.Generation != 2 {
		t.Fatalf("reattached owner = %+v", got)
	}
}

func TestReattachWindowOnlyInterruptsTheDisconnectedGeneration(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManagerWithOptions(transport, ManagerOptions{EnableReattach: true})
	owner := Owner{MAC: "AABBCCDDEEFF", UserID: "u1", DeviceID: "phone", Generation: 1}
	start := StartCommand{Owner: owner, MAC: owner.MAC, SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	if _, err := manager.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	manager.OnOwnerDisconnect(context.Background(), owner)
	if manager.Active(owner.MAC) == nil {
		t.Fatal("owner was interrupted before the reattach window elapsed")
	}
	newOwner := owner
	newOwner.Generation = 2
	if err := manager.ReattachOwner(context.Background(), ReattachCommand{Owner: newOwner, MAC: owner.MAC, SessionID: start.SessionID, CommandID: uuid.NewString()}); err != nil {
		t.Fatalf("reattach: %v", err)
	}
	manager.expireOwnerDisconnect(context.Background(), owner)
	if active := manager.Active(owner.MAC); active == nil || active.Owner != newOwner {
		t.Fatalf("old disconnect terminated replacement owner: %+v", active)
	}

	manager.OnOwnerDisconnect(context.Background(), newOwner)
	manager.expireOwnerDisconnect(context.Background(), newOwner)
	if manager.Active(owner.MAC) != nil {
		t.Fatal("expired reattach window left a session active")
	}
}

func TestDeviceErrorMatchesSessionAndCommandBeforeInterrupting(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	start := StartCommand{Owner: Owner{MAC: "AABBCCDDEEFF", UserID: "u", DeviceID: "p", Generation: 1}, MAC: "AABBCCDDEEFF", SessionID: uuid.NewString(), CommandID: uuid.NewString()}
	_, _ = manager.Start(context.Background(), start)
	if err := manager.OnDeviceError(context.Background(), start.MAC, Event{Action: "meeting.error", SessionID: uuid.NewString(), CommandID: start.CommandID, Code: "WRITE_FAILED"}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("stale session error = %v", err)
	}
	if manager.Active(start.MAC) == nil {
		t.Fatal("stale device error interrupted active session")
	}
	if err := manager.OnDeviceError(context.Background(), start.MAC, Event{Action: "meeting.error", SessionID: start.SessionID, CommandID: uuid.NewString(), Code: "WRITE_FAILED"}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("unrelated command error = %v", err)
	}
	if err := manager.OnDeviceError(context.Background(), start.MAC, Event{Action: "meeting.error", SessionID: start.SessionID, CommandID: start.CommandID, Code: "WRITE_FAILED"}); err != nil {
		t.Fatal(err)
	}
	if manager.Active(start.MAC) != nil {
		t.Fatal("matching device error did not interrupt session")
	}
	last := transport.controls[len(transport.controls)-1]
	if last.Owner != start.Owner || last.Message.Code != "WRITE_FAILED" {
		t.Fatalf("owner error route = %+v", last)
	}
}

func TestShutdownInterruptsEveryActiveMeeting(t *testing.T) {
	transport := &fakeTransport{}
	manager := NewMemoryManager(transport)
	for _, mac := range []string{"AABBCCDDEEFF", "112233445566"} {
		_, _ = manager.Start(context.Background(), StartCommand{Owner: Owner{MAC: mac, UserID: "u", DeviceID: mac, Generation: 1}, MAC: mac, SessionID: uuid.NewString(), CommandID: uuid.NewString()})
	}
	manager.Shutdown(context.Background())
	for _, mac := range []string{"AABBCCDDEEFF", "112233445566"} {
		if manager.Active(mac) != nil {
			t.Fatalf("shutdown left %s active", mac)
		}
	}
}

type sentAudio struct {
	owner   Owner
	payload []byte
}
type fakeTransport struct {
	controls     []ControlRoute
	audio        []sentAudio
	ownerOffline bool
	failDevice   bool
}

func (f *fakeTransport) SendBoundApps(_ context.Context, mac string, message ControlMessage) error {
	f.controls = append(f.controls, ControlRoute{MAC: mac, Message: message})
	return nil
}

func (f *fakeTransport) SendDevice(_ context.Context, mac string, message ControlMessage) error {
	if f.failDevice {
		return errors.New("device queue failed")
	}
	f.controls = append(f.controls, ControlRoute{MAC: mac, Message: message})
	return nil
}
func (f *fakeTransport) SendOwner(_ context.Context, owner Owner, message ControlMessage) error {
	f.controls = append(f.controls, ControlRoute{Owner: owner, Message: message})
	return nil
}
func (f *fakeTransport) SendOwnerAudio(_ context.Context, owner Owner, payload []byte) error {
	if f.ownerOffline {
		return ErrOwnerOffline
	}
	f.audio = append(f.audio, sentAudio{owner: owner, payload: payload})
	return nil
}
func uint32Ptr(value uint32) *uint32 { return &value }

func encodedAudio(t *testing.T, sessionID string, sequence uint32, value byte) []byte {
	t.Helper()
	payload, err := wsprotocol.EncodeMeetingAudio(wsprotocol.MeetingAudioEnvelope{Version: 1, SessionID: uuid.MustParse(sessionID), Sequence: sequence, CaptureTimestampMS: uint64(sequence), Opus: []byte{value}})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func audioSequences(t *testing.T, audio []sentAudio) []uint32 {
	t.Helper()
	result := make([]uint32, 0, len(audio))
	for _, sent := range audio {
		frame, err := wsprotocol.DecodeMeetingAudio(sent.payload)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, frame.Sequence)
	}
	return result
}
