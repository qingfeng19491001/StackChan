package web_socket

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"stackChan/internal/meeting"
	"stackChan/internal/model"
	"stackChan/internal/pairing"
	wsprotocol "stackChan/internal/web_socket/protocol"
)

type wsMeetingTransport struct{}

var meetingManager = meeting.NewMemoryManagerWithOptions(wsMeetingTransport{}, meeting.ManagerOptions{})

// ConfigureMeetingSessionStore replaces the process-local meeting manager at
// startup. A nil store preserves the single-instance, in-memory default.
func ConfigureMeetingSessionStore(store meeting.SessionStore) {
	ConfigureMeetingRuntime(store, meeting.ManagerOptions{})
}

// ConfigureMeetingRuntime selects the durable session store and explicit
// transport behavior before the WebSocket server accepts clients.
func ConfigureMeetingRuntime(store meeting.SessionStore, options meeting.ManagerOptions) {
	if store == nil {
		meetingManager = meeting.NewMemoryManagerWithOptions(wsMeetingTransport{}, options)
		return
	}
	meetingManager = meeting.NewMemoryManagerWithOptionsAndStore(wsMeetingTransport{}, store, options)
}

func controlPayload(message meeting.ControlMessage) *[]byte {
	body := map[string]any{
		"protocolVersion": 1, "action": message.Action,
		"messageId": uuid.NewString(), "commandId": message.CommandID,
		"sessionId": message.SessionID,
	}
	if message.FirstSequence != nil {
		body["firstSequence"] = *message.FirstSequence
	}
	if message.Action == "meeting.stopped" {
		body["lastSequence"] = message.LastSequence
	}
	if message.Reason != "" {
		body["reason"] = message.Reason
	}
	if message.Code != "" {
		body["code"] = message.Code
	}
	if message.MAC != "" {
		body["mac"] = message.MAC
	}
	if message.ExpiresAt != 0 {
		body["expiresAt"] = message.ExpiresAt
	}
	if message.Action == "meeting.start" {
		body["audio"] = map[string]any{
			"codec":           "opus",
			"sampleRate":      16000,
			"channels":        1,
			"frameDurationMs": 60,
		}
	}
	encoded, _ := json.Marshal(body)
	return createMessage(wsprotocol.MeetingControlMessageType, encoded)
}

// deviceBoundControl drops fields the firmware inbound schema rejects.
// meeting.stop is exactly five fields; reason belongs on App-originated stop
// and on meeting.stopped, never on the device-bound command.
func deviceBoundControl(message meeting.ControlMessage) meeting.ControlMessage {
	if message.Action == "meeting.stop" {
		message.Reason = ""
		message.MAC = ""
	}
	return message
}

func (wsMeetingTransport) SendBoundApps(ctx context.Context, mac string, message meeting.ControlMessage) error {
	if err := sendBoundAppsLocal(ctx, mac, message); err == nil {
		return nil
	}
	return currentMeetingCluster().Publish(ctx, clusterDelivery{Kind: clusterToBoundApps, MAC: mac, Control: &message})
}

func sendBoundAppsLocal(ctx context.Context, mac string, message meeting.ControlMessage) error {
	ownerID, bound := pairing.DefaultRepository.OwnerOf(mac)
	if !bound {
		return pairing.ErrNotBound
	}
	frame, messageType := controlPayload(message), websocket.BinaryMessage
	delivered := false
	for _, client := range getAppClients(mac) {
		generation := client.ConnectionGeneration()
		if client.GetUserID() != ownerID || client.GetConn() == nil || !canObserveMeetingState(client, generation) || client.MeetingTicketMAC(generation) != mac {
			continue
		}
		if appSendMessage(ctx, client, &messageType, frame) != model.SendEnqueued {
			return modelSendError{}
		}
		delivered = true
	}
	if !delivered {
		return meeting.ErrOwnerOffline
	}
	return nil
}

func (wsMeetingTransport) SendDevice(ctx context.Context, mac string, message meeting.ControlMessage) error {
	message = deviceBoundControl(message)
	// A local registry entry is authoritative even after its socket has been
	// cleared. Preserve ErrDeviceOffline instead of replacing it with a generic
	// cluster-publish error in the single-instance deployment.
	if getStackChanClient(mac) != nil {
		return sendDeviceLocal(ctx, mac, message)
	}
	cluster := currentMeetingCluster()
	if cluster.NodeID() == "" {
		return meeting.ErrDeviceOffline
	}
	return cluster.Publish(ctx, clusterDelivery{Kind: clusterToDevice, MAC: mac, Control: &message})
}

func sendDeviceLocal(ctx context.Context, mac string, message meeting.ControlMessage) error {
	client := getStackChanClient(mac)
	if client == nil || client.GetConn() == nil {
		return meeting.ErrDeviceOffline
	}
	if !client.SupportsMeetingV1(client.ConnectionGeneration()) {
		return meeting.ErrDeviceOffline
	}
	frame, messageType := controlPayload(deviceBoundControl(message)), websocket.BinaryMessage
	if stackChanSendMessage(ctx, client, &messageType, frame) != model.SendEnqueued {
		return modelSendError{}
	}
	return nil
}

func (wsMeetingTransport) SendOwner(ctx context.Context, owner meeting.Owner, message meeting.ControlMessage) error {
	err := sendOwnerLocal(ctx, owner, message)
	if err == nil {
		return nil
	}
	cluster := currentMeetingCluster()
	if owner.NodeID == "" || owner.NodeID == cluster.NodeID() {
		return err
	}
	return cluster.Publish(ctx, clusterDelivery{Kind: clusterToOwner, Owner: owner, Control: &message})
}

func sendOwnerLocal(ctx context.Context, owner meeting.Owner, message meeting.ControlMessage) error {
	client := findOwnerClient(owner)
	if client == nil || client.GetConn() == nil {
		return meeting.ErrUnauthorized
	}
	frame, messageType := controlPayload(message), websocket.BinaryMessage
	if appSendMessageForGeneration(ctx, client, owner.Generation, &messageType, frame) != model.SendEnqueued {
		return modelSendError{}
	}
	return nil
}

func (wsMeetingTransport) SendOwnerAudio(ctx context.Context, owner meeting.Owner, payload []byte) error {
	err := sendOwnerAudioLocal(ctx, owner, payload)
	if err == nil {
		return nil
	}
	cluster := currentMeetingCluster()
	if owner.NodeID == "" || owner.NodeID == cluster.NodeID() {
		return err
	}
	return cluster.Publish(ctx, clusterDelivery{Kind: clusterAudioOwner, Owner: owner, Audio: append([]byte(nil), payload...)})
}

func sendOwnerAudioLocal(ctx context.Context, owner meeting.Owner, payload []byte) error {
	client := findOwnerClient(owner)
	if client == nil || client.GetConn() == nil {
		return meeting.ErrOwnerOffline
	}
	frame, messageType := createMessage(Opus, payload), websocket.BinaryMessage
	if appSendMessageForGeneration(ctx, client, owner.Generation, &messageType, frame) != model.SendEnqueued {
		return modelSendError{}
	}
	return nil
}

type modelSendError struct{}

func (modelSendError) Error() string { return "websocket enqueue failed" }

func findOwnerClient(owner meeting.Owner) *model.AppClient {
	if owner.NodeID != "" && owner.NodeID != currentMeetingNodeID() {
		return nil
	}
	for _, client := range getAppClients(owner.MAC) {
		if client == nil || client.GetUserID() != owner.UserID || client.GetDeviceId() != owner.DeviceID {
			continue
		}
		_, generation := client.ConnectionSnapshot()
		if generation == owner.Generation && canObserveMeetingState(client, generation) && client.MeetingTicketMAC(generation) == owner.MAC {
			return client
		}
	}
	return nil
}

func sendSelected(ctx context.Context, app *model.AppClient, device *model.StackChanClient) {
	payload, _ := json.Marshal(map[string]any{"protocolVersion": 1, "action": wsprotocol.ActionProtocolSelected, "messageId": uuid.NewString(), "capabilities": []string{"meeting-v1"}})
	frame, messageType := createMessage(wsprotocol.MeetingControlMessageType, payload), websocket.BinaryMessage
	if app != nil {
		appSendMessage(ctx, app, &messageType, frame)
	} else {
		stackChanSendMessage(ctx, device, &messageType, frame)
	}
}

func meetingErrorCode(err error) string {
	switch {
	case errors.Is(err, meeting.ErrSessionBusy):
		return "SESSION_BUSY"
	case errors.Is(err, meeting.ErrUnauthorized):
		return "UNAUTHORIZED"
	case errors.Is(err, meeting.ErrDeviceOffline):
		return "DEVICE_OFFLINE"
	case errors.Is(err, meeting.ErrAudioGap):
		return "AUDIO_GAP"
	case errors.Is(err, meeting.ErrStartTimeout):
		return "START_TIMEOUT"
	case errors.Is(err, pairing.ErrNotBound):
		return "NOT_BOUND"
	case errors.Is(err, meeting.ErrSessionNotFound):
		return "SESSION_NOT_FOUND"
	default:
		return "DEVICE_ERROR"
	}
}

func sendMeetingError(ctx context.Context, client *model.AppClient, request wsprotocol.MeetingControl, err error) {
	code := meetingErrorCode(err)
	logger.Warningf(ctx, "[SCMEET-DIAG] app.error action=%s session=%s code=%s", request.Action, request.SessionID, code)
	frame := controlPayload(meeting.ControlMessage{
		Action:    "meeting.error",
		SessionID: request.SessionID,
		CommandID: request.CommandID,
		Code:      code,
	})
	messageType := websocket.BinaryMessage
	appSendMessage(ctx, client, &messageType, frame)
}

func notifyMeetingOwnerError(ctx context.Context, mac string, request wsprotocol.MeetingControl, err error) {
	session := meetingManager.Active(mac)
	if session == nil {
		return
	}
	// Device retries can surface an event from an older meeting after the
	// owner has already started a new one. Never leak that stale error into the
	// current owner's socket: the App would treat it as the active command and
	// abort a healthy start/reconnect.
	if request.SessionID != "" && request.SessionID != session.SessionID {
		return
	}
	if request.CommandID != "" && request.CommandID != session.StartCommandID && request.CommandID != session.StopCommandID {
		return
	}
	if request.SessionID == "" {
		request.SessionID = session.SessionID
	}
	code := meetingErrorCode(err)
	logger.Warningf(ctx, "[SCMEET-DIAG] owner.error mac=%s session=%s code=%s", mac, request.SessionID, code)
	_ = (wsMeetingTransport{}).SendOwner(ctx, session.Owner, meeting.ControlMessage{
		Action:    "meeting.error",
		SessionID: request.SessionID,
		CommandID: request.CommandID,
		Code:      code,
	})
}

func handleDeviceMeetingControl(ctx context.Context, client *model.StackChanClient, payload []byte) bool {
	message, err := wsprotocol.ParseMeetingControlForDirection(payload, wsprotocol.DirectionDevice)
	if err != nil {
		logger.Warningf(ctx, "[SCMEET-DIAG] device.parse_failed mac=%s error=%v", client.GetMac(), err)
		return true
	}
	logMeetingControl(ctx, "device", message)
	if message.Action == wsprotocol.ActionProtocolHello && message.SupportsMeetingV1() {
		generation := client.ConnectionGeneration()
		client.SelectMeetingV1(generation)
		sendSelected(ctx, nil, client)
		return true
	}
	if !client.SupportsMeetingV1(client.ConnectionGeneration()) {
		return true
	}
	if message.Action == "meeting.start-requested" {
		if err := meetingManager.RequestStart(ctx, meeting.DeviceStartRequest{MAC: client.GetMac(), SessionID: message.SessionID, CommandID: message.CommandID}); err != nil {
			_ = (wsMeetingTransport{}).SendDevice(ctx, client.GetMac(), meeting.ControlMessage{Action: "meeting.error", SessionID: message.SessionID, CommandID: message.CommandID, Code: meetingErrorCode(err)})
		}
	} else if message.Action == "meeting.stop-requested" {
		if err := meetingManager.RequestStop(ctx, meeting.DeviceStopRequest{MAC: client.GetMac(), SessionID: message.SessionID, CommandID: message.CommandID, Reason: message.Reason}); err != nil {
			_ = (wsMeetingTransport{}).SendDevice(ctx, client.GetMac(), meeting.ControlMessage{Action: "meeting.error", SessionID: message.SessionID, CommandID: message.CommandID, Code: meetingErrorCode(err)})
		}
	} else if message.Action == "meeting.started" || message.Action == "meeting.stopped" {
		if err := meetingManager.OnDeviceEvent(ctx, client.GetMac(), meeting.Event{Action: message.Action, SessionID: message.SessionID, CommandID: message.CommandID, FirstSequence: message.FirstSequence, LastSequence: message.LastSequence, Reason: message.Reason}); err != nil {
			logger.Warningf(ctx, "[SCMEET-DIAG] device.event_rejected action=%s session=%s lastSequence=%v code=%s", message.Action, message.SessionID, formatOptionalUint32(message.LastSequence), meetingErrorCode(err))
			notifyMeetingOwnerError(ctx, client.GetMac(), message, err)
		}
	} else if message.Action == "meeting.error" {
		session := meetingManager.Active(client.GetMac())
		if session == nil || session.SessionID != message.SessionID || (message.CommandID != session.StartCommandID && message.CommandID != session.StopCommandID) {
			return true
		}
		_ = meetingManager.OnDeviceError(ctx, client.GetMac(), meeting.Event{Action: "meeting.error", SessionID: message.SessionID, CommandID: message.CommandID, Code: message.Code})
	}
	return true
}

func handleAppMeetingControl(ctx context.Context, client *model.AppClient, payload []byte) bool {
	message, err := wsprotocol.ParseMeetingControlForDirection(payload, wsprotocol.DirectionApp)
	if err != nil {
		logger.Warningf(ctx, "[SCMEET-DIAG] app.parse_failed error=%v", err)
		return true
	}
	logMeetingControl(ctx, "app", message)
	if message.Action == wsprotocol.ActionProtocolHello && message.SupportsMeetingV1() {
		generation := client.ConnectionGeneration()
		client.SelectMeetingV1(generation)
		sendSelected(ctx, client, nil)
		return true
	}
	if !client.SupportsMeetingV1(client.ConnectionGeneration()) {
		sendMeetingError(ctx, client, message, meeting.ErrUnauthorized)
		return true
	}
	if client.MeetingTicketMAC(client.ConnectionGeneration()) == "" {
		sendMeetingError(ctx, client, message, meeting.ErrUnauthorized)
		return true
	}
	if normalized, normalizeErr := pairing.NormalizeMAC(message.MAC); normalizeErr != nil || normalized != client.MeetingTicketMAC(client.ConnectionGeneration()) {
		sendMeetingError(ctx, client, message, meeting.ErrUnauthorized)
		return true
	}
	if err := authorizeAppMeetingCommand(pairing.DefaultRepository, client, client.ConnectionGeneration(), message.MAC); err != nil {
		sendMeetingError(ctx, client, message, meeting.ErrUnauthorized)
		return true
	}
	device := getStackChanClient(message.MAC)
	if device == nil || !bothMeetingV1Selected(client, client.ConnectionGeneration(), device, device.ConnectionGeneration()) {
		sendMeetingError(ctx, client, message, meeting.ErrDeviceOffline)
		return true
	}
	owner := meeting.Owner{NodeID: currentMeetingNodeID(), MAC: client.GetMac(), UserID: client.GetUserID(), DeviceID: client.GetDeviceId(), Generation: client.ConnectionGeneration()}
	switch message.Action {
	case "meeting.start":
		if _, err := meetingManager.Start(ctx, meeting.StartCommand{Owner: owner, MAC: message.MAC, SessionID: message.SessionID, CommandID: message.CommandID}); err != nil {
			sendMeetingError(ctx, client, message, err)
		}
	case "meeting.stop":
		if _, err := meetingManager.Stop(ctx, meeting.StopCommand{Owner: owner, MAC: message.MAC, SessionID: message.SessionID, CommandID: message.CommandID, Reason: message.Reason}); err != nil {
			sendMeetingError(ctx, client, message, err)
		}
	case "meeting.accept":
		if _, err := meetingManager.AcceptRequestedStart(ctx, meeting.AcceptStartCommand{Owner: owner, MAC: message.MAC, SessionID: message.SessionID, CommandID: message.CommandID}); err != nil {
			sendMeetingError(ctx, client, message, err)
		}
	case "meeting.reattach":
		var lastReceivedSequence any = "none"
		if message.LastReceivedSequence != nil {
			lastReceivedSequence = *message.LastReceivedSequence
		}
		logger.Infof(ctx, "[SCMEET-REATTACH] requested mac=%s generation=%d lastReceivedSequence=%v", message.MAC, owner.Generation, lastReceivedSequence)
		if err := meetingManager.ReattachOwner(ctx, meeting.ReattachCommand{Owner: owner, MAC: message.MAC, SessionID: message.SessionID, CommandID: message.CommandID, LastReceivedSequence: message.LastReceivedSequence}); err != nil {
			logger.Warningf(ctx, "[SCMEET-REATTACH] rejected mac=%s generation=%d code=%s", message.MAC, owner.Generation, meetingErrorCode(err))
			sendMeetingError(ctx, client, message, err)
		} else {
			logger.Infof(ctx, "[SCMEET-REATTACH] accepted mac=%s generation=%d", message.MAC, owner.Generation)
		}
	}
	return true
}
