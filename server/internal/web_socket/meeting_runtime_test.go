package web_socket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"stackChan/internal/meeting"
	"stackChan/internal/model"
	"stackChan/internal/pairing"
	wsprotocol "stackChan/internal/web_socket/protocol"
)

func TestStartCommandMatchesFirmwareAudioSchema(t *testing.T) {
	frame := controlPayload(meeting.ControlMessage{Action: "meeting.start", SessionID: uuid.NewString(), CommandID: uuid.NewString()})
	var body map[string]any
	if err := json.Unmarshal((*frame)[5:], &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 6 {
		t.Fatalf("firmware requires 6 start fields: %#v", body)
	}
	audio := body["audio"].(map[string]any)
	if len(audio) != 4 || audio["codec"] != "opus" || audio["sampleRate"] != float64(16000) || audio["channels"] != float64(1) || audio["frameDurationMs"] != float64(60) {
		t.Fatalf("audio does not match firmware's exact four-field schema: %#v", audio)
	}
}

func TestDeviceRequestedMeetingStartsOnlyAfterBoundAuroAccepts(t *testing.T) {
	deviceServer, devicePeer := websocketPair(t)
	defer devicePeer.Close()
	appServer, appPeer := websocketPair(t)
	defer appPeer.Close()

	mac := "AABBCCDDEEFF"
	device := model.NewStackChanClient(mac, deviceServer, nil, nil, false)
	defer device.CloseWriterCoroutine()
	device.SelectMeetingV1(device.ConnectionGeneration())
	stackChanClientPool.Store(mac, device)
	defer stackChanClientPool.Delete(mac)

	app := model.NewAppClient(mac, appServer, "phone")
	defer app.CloseWriterCoroutine()
	app.SetUserID("owner")
	app.SetMeetingAuthorization("owner", "phone", app.ConnectionGeneration())
	app.SelectMeetingV1(app.ConnectionGeneration())
	addAppClient(app)
	defer appClientPool.Delete(mac)

	previousRepository := pairing.DefaultRepository
	repository := pairing.NewMemoryRepository(time.Now)
	nonce, _ := repository.IssueNonce(mac, device.ConnectionGeneration())
	if err := repository.Bind("owner", mac, nonce.Value, device.ConnectionGeneration()); err != nil {
		t.Fatal(err)
	}
	pairing.DefaultRepository = repository
	defer func() { pairing.DefaultRepository = previousRepository }()
	previousManager := meetingManager
	meetingManager = meeting.NewMemoryManager(wsMeetingTransport{})
	defer func() { meetingManager = previousManager }()

	sessionID, commandID := uuid.NewString(), uuid.NewString()
	requested, _ := json.Marshal(wsprotocol.MeetingControl{
		ProtocolVersion: 1, Action: "meeting.start-requested", MessageID: uuid.NewString(),
		SessionID: sessionID, CommandID: commandID,
	})
	handleDeviceMeetingControl(context.Background(), device, requested)
	offer := parseControlMap(t, readBinaryMessage(t, appPeer))
	if offer["action"] != "meeting.start-offered" || offer["mac"] != mac || offer["expiresAt"] == nil {
		t.Fatalf("offer = %+v", offer)
	}

	accepted, _ := json.Marshal(wsprotocol.MeetingControl{
		ProtocolVersion: 1, Action: "meeting.accept", MessageID: uuid.NewString(),
		SessionID: sessionID, CommandID: commandID, MAC: mac,
	})
	handleAppMeetingControl(context.Background(), app, accepted)
	start := parseControlMap(t, readBinaryMessage(t, devicePeer))
	if start["action"] != "meeting.start" || start["sessionId"] != sessionID {
		t.Fatalf("start = %+v", start)
	}
	first := uint32(0)
	started, _ := json.Marshal(wsprotocol.MeetingControl{
		ProtocolVersion: 1, Action: "meeting.started", MessageID: uuid.NewString(),
		SessionID: sessionID, CommandID: commandID, FirstSequence: &first,
	})
	handleDeviceMeetingControl(context.Background(), device, started)
	readBinaryMessage(t, appPeer)

	stopCommandID := uuid.NewString()
	stopRequested, _ := json.Marshal(wsprotocol.MeetingControl{
		ProtocolVersion: 1, Action: "meeting.stop-requested", MessageID: uuid.NewString(),
		SessionID: sessionID, CommandID: stopCommandID, Reason: "user",
	})
	handleDeviceMeetingControl(context.Background(), device, stopRequested)
	stop := parseControlMap(t, readBinaryMessage(t, appPeer))
	if stop["action"] != "meeting.stop" || stop["commandId"] != stopCommandID {
		t.Fatalf("stop = %+v", stop)
	}
}

func TestBusyMeetingStartImmediatelyReturnsTypedError(t *testing.T) {
	deviceServer, devicePeer := websocketPair(t)
	defer devicePeer.Close()
	appServer, appPeer := websocketPair(t)
	defer appPeer.Close()

	mac := "AABBCCDDEEFF"
	device := model.NewStackChanClient(mac, deviceServer, nil, nil, false)
	defer device.CloseWriterCoroutine()
	if !device.SelectMeetingV1(device.ConnectionGeneration()) {
		t.Fatal("failed to select meeting-v1 for test device")
	}
	stackChanClientPool.Store(mac, device)
	defer stackChanClientPool.Delete(mac)

	previousManager := meetingManager
	meetingManager = meeting.NewMemoryManager(wsMeetingTransport{})
	defer func() { meetingManager = previousManager }()

	first := meeting.StartCommand{
		Owner:     meeting.Owner{UserID: "owner-1", DeviceID: "phone-1"},
		MAC:       mac,
		SessionID: uuid.NewString(),
		CommandID: uuid.NewString(),
	}
	if _, err := meetingManager.Start(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	readBinaryMessage(t, devicePeer)

	app := model.NewAppClient(mac, appServer, "phone-2")
	defer app.CloseWriterCoroutine()
	app.SetUserID("owner-2")
	app.SetMeetingAuthorization("owner-2", "phone-2", app.ConnectionGeneration())
	if !app.SelectMeetingV1(app.ConnectionGeneration()) {
		t.Fatal("failed to select meeting-v1 for test app")
	}
	previousRepository := pairing.DefaultRepository
	repository := pairing.NewMemoryRepository(time.Now)
	nonce, _ := repository.IssueNonce(mac, device.ConnectionGeneration())
	if err := repository.Bind("owner-2", mac, nonce.Value, device.ConnectionGeneration()); err != nil {
		t.Fatal(err)
	}
	pairing.DefaultRepository = repository
	defer func() { pairing.DefaultRepository = previousRepository }()

	request := wsprotocol.MeetingControl{
		ProtocolVersion: 1,
		Action:          "meeting.start",
		MessageID:       uuid.NewString(),
		CommandID:       uuid.NewString(),
		SessionID:       uuid.NewString(),
		MAC:             mac,
		Audio:           &wsprotocol.MeetingAudioParameters{Codec: "opus", SampleRate: 16000, Channels: 1, FrameDurationMS: 60},
	}
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	handleAppMeetingControl(context.Background(), app, payload)

	responseFrame := readBinaryMessage(t, appPeer)
	messageType, responsePayload, err := wsprotocol.ParseBinaryMessage(responseFrame)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != wsprotocol.MeetingControlMessageType {
		t.Fatalf("response type = %d", messageType)
	}
	response, err := wsprotocol.ParseMeetingControl(responsePayload)
	if err != nil {
		t.Fatal(err)
	}
	if response.Action != "meeting.error" || response.Code != "SESSION_BUSY" {
		t.Fatalf("response = %+v", response)
	}
	if response.SessionID != request.SessionID || response.CommandID != request.CommandID {
		t.Fatalf("error does not identify rejected command: %+v", response)
	}
}

func TestMeetingStartSentToDeviceIncludesFixedAudioContract(t *testing.T) {
	deviceServer, devicePeer := websocketPair(t)
	defer devicePeer.Close()

	mac := "AABBCCDDEEFF"
	device := model.NewStackChanClient(mac, deviceServer, nil, nil, false)
	defer device.CloseWriterCoroutine()
	if !device.SelectMeetingV1(device.ConnectionGeneration()) {
		t.Fatal("failed to select meeting-v1 for test device")
	}
	stackChanClientPool.Store(mac, device)
	defer stackChanClientPool.Delete(mac)

	manager := meeting.NewMemoryManager(wsMeetingTransport{})
	command := meeting.StartCommand{
		Owner:     meeting.Owner{UserID: "owner", DeviceID: "phone"},
		MAC:       mac,
		SessionID: uuid.NewString(),
		CommandID: uuid.NewString(),
	}
	if _, err := manager.Start(context.Background(), command); err != nil {
		t.Fatal(err)
	}

	control := parseControlMap(t, readBinaryMessage(t, devicePeer))
	audio, ok := control["audio"].(map[string]any)
	if !ok {
		t.Fatalf("meeting.start audio contract missing: %+v", control)
	}
	expected := map[string]any{
		"codec": "opus", "sampleRate": float64(16000), "channels": float64(1),
		"frameDurationMs": float64(60),
	}
	for field, want := range expected {
		if got := audio[field]; got != want {
			t.Fatalf("audio.%s = %#v, want %#v", field, got, want)
		}
	}
}

func TestInvalidDeviceStopBarrierImmediatelyNotifiesOwner(t *testing.T) {
	deviceServer, devicePeer := websocketPair(t)
	defer devicePeer.Close()
	appServer, appPeer := websocketPair(t)
	defer appPeer.Close()

	mac := "AABBCCDDEEFF"
	device := model.NewStackChanClient(mac, deviceServer, nil, nil, false)
	defer device.CloseWriterCoroutine()
	if !device.SelectMeetingV1(device.ConnectionGeneration()) {
		t.Fatal("failed to select meeting-v1 for test device")
	}
	stackChanClientPool.Store(mac, device)
	defer stackChanClientPool.Delete(mac)

	app := model.NewAppClient(mac, appServer, "phone")
	defer app.CloseWriterCoroutine()
	app.SetUserID("owner")
	app.SetMeetingAuthorization("owner", "phone", app.ConnectionGeneration())
	app.SelectMeetingV1(app.ConnectionGeneration())
	addAppClient(app)
	defer appClientPool.Delete(mac)

	previousManager := meetingManager
	meetingManager = meeting.NewMemoryManager(wsMeetingTransport{})
	defer func() { meetingManager = previousManager }()

	start := meeting.StartCommand{
		Owner:     meeting.Owner{MAC: mac, UserID: "owner", DeviceID: "phone", Generation: app.ConnectionGeneration()},
		MAC:       mac,
		SessionID: uuid.NewString(),
		CommandID: uuid.NewString(),
	}
	if _, err := meetingManager.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	readBinaryMessage(t, devicePeer)
	if err := meetingManager.OnDeviceEvent(context.Background(), mac, meeting.Event{
		Action: "meeting.started", SessionID: start.SessionID, CommandID: start.CommandID,
		FirstSequence: uint32Pointer(0),
	}); err != nil {
		t.Fatal(err)
	}
	readBinaryMessage(t, appPeer)

	audioPayload, err := wsprotocol.EncodeMeetingAudio(wsprotocol.MeetingAudioEnvelope{
		Version: wsprotocol.MeetingProtocolVersion, SessionID: uuid.MustParse(start.SessionID),
		Sequence: 0, CaptureTimestampMS: 1, Opus: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := meetingManager.OnAudio(context.Background(), mac, audioPayload); err != nil {
		t.Fatal(err)
	}
	readBinaryMessage(t, appPeer)

	stop := meeting.StopCommand{
		Owner: start.Owner, MAC: mac, SessionID: start.SessionID,
		CommandID: uuid.NewString(), Reason: "user",
	}
	if _, err := meetingManager.Stop(context.Background(), stop); err != nil {
		t.Fatal(err)
	}
	readBinaryMessage(t, devicePeer)

	invalidLast := uint32(2)
	deviceEvent := wsprotocol.MeetingControl{
		ProtocolVersion: 1, Action: "meeting.stopped", MessageID: uuid.NewString(),
		SessionID: start.SessionID, CommandID: stop.CommandID, LastSequence: &invalidLast,
	}
	payload, err := json.Marshal(deviceEvent)
	if err != nil {
		t.Fatal(err)
	}
	handleDeviceMeetingControl(context.Background(), device, payload)

	response := parseControlMap(t, readBinaryMessage(t, appPeer))
	if response["action"] != "meeting.error" || response["code"] != "AUDIO_GAP" {
		t.Fatalf("response = %+v", response)
	}
	if response["sessionId"] != start.SessionID || response["commandId"] != stop.CommandID {
		t.Fatalf("error does not identify invalid barrier: %+v", response)
	}
}

func TestAudioSequenceGapImmediatelyNotifiesOwner(t *testing.T) {
	deviceServer, devicePeer := websocketPair(t)
	defer devicePeer.Close()
	appServer, appPeer := websocketPair(t)
	defer appPeer.Close()

	mac := "AABBCCDDEEFF"
	device := model.NewStackChanClient(mac, deviceServer, nil, nil, false)
	defer device.CloseWriterCoroutine()
	if !device.SelectMeetingV1(device.ConnectionGeneration()) {
		t.Fatal("failed to select meeting-v1 for test device")
	}
	stackChanClientPool.Store(mac, device)
	defer stackChanClientPool.Delete(mac)

	app := model.NewAppClient(mac, appServer, "phone")
	defer app.CloseWriterCoroutine()
	app.SetUserID("owner")
	app.SetMeetingAuthorization("owner", "phone", app.ConnectionGeneration())
	app.SelectMeetingV1(app.ConnectionGeneration())
	addAppClient(app)
	defer appClientPool.Delete(mac)

	previousManager := meetingManager
	meetingManager = meeting.NewMemoryManager(wsMeetingTransport{})
	defer func() { meetingManager = previousManager }()

	start := meeting.StartCommand{
		Owner:     meeting.Owner{MAC: mac, UserID: "owner", DeviceID: "phone", Generation: app.ConnectionGeneration()},
		MAC:       mac,
		SessionID: uuid.NewString(),
		CommandID: uuid.NewString(),
	}
	if _, err := meetingManager.Start(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	readBinaryMessage(t, devicePeer)
	if err := meetingManager.OnDeviceEvent(context.Background(), mac, meeting.Event{
		Action: "meeting.started", SessionID: start.SessionID, CommandID: start.CommandID,
		FirstSequence: uint32Pointer(0),
	}); err != nil {
		t.Fatal(err)
	}
	readBinaryMessage(t, appPeer)

	validPayload := encodeMeetingAudio(t, start.SessionID, 0)
	validFrame := createMessage(Opus, validPayload)
	messageType := websocket.BinaryMessage
	readStackChanMessage(context.Background(), device, &messageType, validFrame)
	readBinaryMessage(t, appPeer)

	gapPayload := encodeMeetingAudio(t, start.SessionID, 2)
	gapFrame := createMessage(Opus, gapPayload)
	readStackChanMessage(context.Background(), device, &messageType, gapFrame)

	response := parseControlMap(t, readBinaryMessage(t, appPeer))
	if response["action"] != "meeting.error" || response["code"] != "AUDIO_GAP" {
		t.Fatalf("response = %+v", response)
	}
	if response["sessionId"] != start.SessionID {
		t.Fatalf("error does not identify interrupted session: %+v", response)
	}
}

func websocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	serverConnections := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		serverConnections <- connection
		<-done
		_ = connection.Close()
	}))
	t.Cleanup(func() {
		close(done)
		server.Close()
	})

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	peer, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return <-serverConnections, peer
}

func readBinaryMessage(t *testing.T, connection *websocket.Conn) []byte {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	messageType, payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("message type = %d", messageType)
	}
	return payload
}

func parseControlMap(t *testing.T, frame []byte) map[string]any {
	t.Helper()
	messageType, payload, err := wsprotocol.ParseBinaryMessage(frame)
	if err != nil {
		t.Fatal(err)
	}
	if messageType != wsprotocol.MeetingControlMessageType {
		t.Fatalf("message type = %d", messageType)
	}
	var control map[string]any
	if err := json.Unmarshal(payload, &control); err != nil {
		t.Fatal(err)
	}
	return control
}

func uint32Pointer(value uint32) *uint32 { return &value }

func encodeMeetingAudio(t *testing.T, sessionID string, sequence uint32) []byte {
	t.Helper()
	payload, err := wsprotocol.EncodeMeetingAudio(wsprotocol.MeetingAudioEnvelope{
		Version: wsprotocol.MeetingProtocolVersion, SessionID: uuid.MustParse(sessionID),
		Sequence: sequence, CaptureTimestampMS: uint64(sequence + 1), Opus: []byte{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
