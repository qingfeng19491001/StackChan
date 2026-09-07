/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package web_socket

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"os"
	"stackChan/internal/meeting"
	"stackChan/internal/meetingmetrics"
	"stackChan/internal/model"
	"stackChan/internal/pairing"
	"stackChan/internal/service"
	wsprotocol "stackChan/internal/web_socket/protocol"
	"stackChan/utility"
	"strings"
	"sync"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gorilla/websocket"
)

const (
	Opus          byte = 0x01
	Jpeg          byte = 0x02
	ControlAvatar byte = 0x03
	ControlMotion byte = 0x04
	OnCamera      byte = 0x05
	OffCamera     byte = 0x06

	TextMessage byte = 0x07
	RequestCall byte = 0x09
	RefuseCall  byte = 0x0A
	AgreeCall   byte = 0x0B
	HangupCall  byte = 0x0C

	UpdateDeviceName byte = 0x0D
	GetDeviceName    byte = 0x0E

	inCall byte = 0x0F

	ping byte = 0x10
	pong byte = 0x11

	OnPhoneScreen    byte = 0x12
	OffPhoneScreen   byte = 0x13
	Dance            byte = 0x14
	GetAvatarPosture byte = 0x15

	DeviceOffline byte = 0x16
	DeviceOnline  byte = 0x17

	OnAudio  byte = 0x18
	OffAudio byte = 0x19

	AimedTakePhoto byte = 0x1A
	MeetingControl byte = 0x1B
)

var (
	wsUpGrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return meetingRequestPolicy.Validate(r) == nil },
		Error: func(w http.ResponseWriter, r *http.Request, status int, reason error) {
			logger.Errorf(r.Context(), "WebSocket Upgrade failed: %v", reason)
		},
	}
	logger              = g.Log()
	stackChanClientPool = sync.Map{}
	appClientPool       = sync.Map{}
	appClientMu         sync.Mutex
)

// GetMac get MAC address from request header
func GetMac(r *ghttp.Request) (string, error) {
	if token := r.Header.Get(model.Authorization); token != "" {
		if strings.HasPrefix(token, "Device ") {
			provided := strings.TrimSpace(strings.TrimPrefix(token, "Device "))
			if mac, ok := deviceCredentialMAC(provided, time.Now()); ok {
				return mac, nil
			}
			return "", errors.New("invalid device credential")
		}
		decodedToken, err := base64.StdEncoding.DecodeString(token)
		if err != nil {
			logger.Errorf(r.Context(), "Error base64 decoding token: %v", err)
			return "", err
		}
		decrypted, err := utility.RSADecrypt(decodedToken)
		if err != nil {
			logger.Errorf(r.Context(), "Error decrypting token: %v", err)
			return "", err
		}
		mac, err := wsprotocol.ParseAuthorizationClaims(string(decrypted), time.Now())
		if err != nil {
			return "", err
		}
		return pairing.NormalizeMAC(mac)
	}
	return "", nil
}

func DeviceConnectionGeneration(mac string) (uint64, bool) {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return 0, false
	}
	cluster := currentMeetingCluster()
	if cluster.NodeID() != "" {
		return cluster.DeviceGeneration(context.Background(), mac)
	}
	client := getStackChanClient(mac)
	if client == nil || client.GetConn() == nil {
		return 0, false
	}
	return client.ConnectionGeneration(), true
}

// Handler WebSocket handler function
func Handler(r *ghttp.Request) {
	ctx := r.Context()
	if err := authorizeMeetingUpgrade(r.Request); err != nil {
		status := http.StatusForbidden
		if errors.Is(err, ErrUpgradeRateLimited) {
			status = http.StatusTooManyRequests
		}
		r.Response.WriteHeader(status)
		r.Response.Write("WebSocket request rejected")
		return
	}
	deviceType := r.Get("deviceType").String()
	if deviceType != "StackChan" && deviceType != "App" {
		r.Response.WriteHeader(http.StatusBadRequest)
		r.Response.Write("Invalid deviceType.")
		return
	}
	var appTicketClaims *pairing.TicketClaims
	var mac string
	var err error
	if deviceType == "App" && strings.HasPrefix(r.Header.Get(model.Authorization), "Bearer ") {
		claims, ticketErr := pairing.DefaultRepository.ConsumeTicket(strings.TrimSpace(strings.TrimPrefix(r.Header.Get(model.Authorization), "Bearer ")))
		if ticketErr == nil && claims.DeviceID == r.Get("deviceId").String() && claims.Role == "app" {
			claims.MAC, err = pairing.NormalizeMAC(claims.MAC)
			if err != nil {
				r.Response.WriteHeader(http.StatusUnauthorized)
				r.Response.Write("Unauthorized")
				return
			}
			appTicketClaims, mac = &claims, claims.MAC
		} else {
			err = errors.New("invalid websocket ticket")
		}
	} else {
		mac, err = GetMac(r)
	}
	if err != nil || mac == "" {
		r.Response.WriteHeader(http.StatusUnauthorized) // Return 401
		r.Response.Write("Unauthorized: invalid or missing MAC")
		return
	}
	ws, err := wsUpGrader.Upgrade(r.Response.Writer, r.Request, nil)
	if err != nil {
		r.Response.Write(err.Error())
		return
	}
	// The outer framing and the meeting decoder both impose smaller payload
	// contracts. Keep a hard transport ceiling as a final DoS boundary.
	ws.SetReadLimit(4 << 20)

	if deviceType == "StackChan" {
		isHave := false
		var client *model.StackChanClient
		var connectionGeneration uint64

		stackChanClientPool.Range(func(key, value any) bool {
			macAddr := key.(string)
			stackChanClient := value.(*model.StackChanClient)

			if macAddr == mac {
				isHave = true
				client = stackChanClient
				previous, generation := client.SwapConnection(ws)
				connectionGeneration = generation
				if previous != nil && previous != ws {
					// A device attachment cannot resume an existing recording. End
					// the old session before accepting capability negotiation from
					// the replacement generation.
					meetingManager.OnDeviceDisconnect(ctx, mac)
					meetingmetrics.Default.IncReconnect()
					_ = previous.Close()
				}
				if client.GetCallAppClient() != nil {
					reconnectMsg := createStringMessage(TextMessage, "The equipment has been reconnected.")
					stackChanSendMessage(ctx, client, new(websocket.BinaryMessage), reconnectMsg)
				}
				if len(client.GetCameraSubscriptionList()) > 0 {
					onMsg := createMessage(OnCamera, nil)
					stackChanSendMessage(ctx, client, new(websocket.BinaryMessage), onMsg)
				}
				if len(client.GetAudioSubscriptionList()) > 0 {
					onMsg := createMessage(OnAudio, nil)
					stackChanSendMessage(ctx, client, new(websocket.BinaryMessage), onMsg)
				}
				client.SetLastTime(time.Now())
				return false
			}
			return true
		})

		if !isHave {
			client = model.NewStackChanClient(mac, ws, make([]*model.AppClient, 0), nil, false)
			connectionGeneration = client.ConnectionGeneration()
			addStackChenClient(ctx, client)
		}
		presenceGeneration := connectionGeneration
		if cluster := currentMeetingCluster(); cluster.NodeID() != "" {
			presenceGeneration, err = cluster.RegisterDevice(ctx, mac)
			if err != nil {
				logger.Errorf(ctx, "Register StackChan presence failed: mac=%s error=%v", mac, err)
				client.ClearConnection(connectionGeneration)
				_ = ws.Close()
				return
			}
		}
		meetingmetrics.Default.SetOnlineDevices(int64(connectedDeviceCount()))

		// send Online
		onlineMsg := createStringMessage(DeviceOnline, "Your StackChan has been launched.")
		msgType := websocket.BinaryMessage
		// Notify App
		appClients := getAppClients(client.GetMac())
		for _, appClient := range appClients {
			if canObserveMeetingState(appClient, appClient.ConnectionGeneration()) {
				appSendMessage(ctx, appClient, &msgType, onlineMsg)
			}
		}

		logger.Info(ctx, "There is a StackChen connected to the service.", client.GetMac())
		defer func() {
			logger.Info(ctx, "There is a StackChan that has disconnected.", mac, deviceType)
			_ = ws.Close()
			if client.ClearConnection(connectionGeneration) {
				if cluster := currentMeetingCluster(); cluster.NodeID() != "" {
					_ = cluster.UnregisterDevice(context.Background(), mac, presenceGeneration)
				}
				meetingManager.OnDeviceDisconnect(ctx, mac)
				meetingmetrics.Default.SetOnlineDevices(int64(connectedDeviceCount()))
			}
		}()
		limiter := newConnectionRateLimiter(nil, defaultConnectionRateLimits)
		for {
			messageType, msg, err := ws.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					logger.Infof(ctx, "StackChan Normal disconnection: mac=%s, deviceType=%s, Reason=%v", mac, deviceType, err)
					break
				}

				if ne, ok := errors.AsType[net.Error](err); ok && ne.Temporary() {
					logger.Infof(ctx, "StackChan Temporary network error. Continue reading.: mac=%s,deviceType=%s,Error=%v", mac, deviceType, err)
					continue
				}

				logger.Errorf(ctx, "StackChan Abnormal disconnection: mac=%s, deviceType=%s, Error=%v", mac, deviceType, err)
				break
			}
			if client.ConnectionGeneration() != connectionGeneration {
				break
			}
			if !allowInbound(limiter, messageType, msg) {
				logger.Warningf(ctx, "StackChan message rate limit exceeded: mac=%s", mac)
				break
			}
			client.SetLastTime(time.Now())
			if cluster := currentMeetingCluster(); cluster.NodeID() != "" {
				if err := cluster.TouchDevice(ctx, mac, presenceGeneration); err != nil {
					logger.Errorf(ctx, "Refresh StackChan presence failed: mac=%s error=%v", mac, err)
					break
				}
			}
			readStackChanMessage(ctx, client, &messageType, &msg)
		}
	} else if deviceType == "App" {
		deviceId := r.Get("deviceId").String()
		if deviceId == "" {
			r.Response.Write("The deviceId parameter in the App end is empty.")
			return
		}
		var client *model.AppClient
		var connectionGeneration uint64
		found := false
		clients := getAppClients(mac)
		for _, appClient := range clients {
			if appClient.GetDeviceId() == deviceId && appClient.GetMac() == mac {
				// Already available. Update the connection.
				client = appClient
				previous, generation := client.SwapConnection(ws)
				connectionGeneration = generation
				if previous != nil && previous != ws {
					// Start the bounded owner reattach window for the attachment
					// that was replaced. A newly ticketed generation must still send
					// meeting.reattach; merely opening a socket does not preserve it.
					meetingManager.OnOwnerDisconnect(ctx, meeting.Owner{
						NodeID: currentMeetingNodeID(), MAC: mac,
						UserID: client.GetUserID(), DeviceID: client.GetDeviceId(),
						Generation: generation - 1,
					})
					meetingmetrics.Default.IncReconnect()
					_ = previous.Close()
				}
				client.SetLastTime(time.Now())
				found = true
				break
			}
		}
		if !found {
			client = model.NewAppClient(mac, ws, deviceId)
			connectionGeneration = client.ConnectionGeneration()
			addAppClient(client)
		}
		if appTicketClaims != nil {
			client.SetUserID(appTicketClaims.UserID)
			if !client.AuthorizeMeetingTicket(mac, connectionGeneration) {
				r.Response.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		logger.Info(ctx, "There is an App connected to the service.", client.GetMac())

		// Presence is meeting metadata: do not disclose it to a legacy or an
		// unauthorized App connection.
		if canObserveMeetingState(client, connectionGeneration) {
			if _, online := DeviceConnectionGeneration(client.GetMac()); !online {
				offlineMsg := createStringMessage(DeviceOffline, "Your StackChan is offline.")
				appSendMessage(ctx, client, new(websocket.BinaryMessage), offlineMsg)
			} else {
				onlineMsg := createStringMessage(DeviceOnline, "Your StackChan has been launched.")
				appSendMessage(ctx, client, new(websocket.BinaryMessage), onlineMsg)
			}
		}

		defer func() {
			logger.Info(ctx, "There is an App that has disconnected.", mac, deviceType)
			_ = ws.Close()
			if client.ClearConnection(connectionGeneration) {
				meetingManager.OnOwnerDisconnect(ctx, meeting.Owner{NodeID: currentMeetingNodeID(), MAC: mac, UserID: client.GetUserID(), DeviceID: client.GetDeviceId(), Generation: connectionGeneration})
			}
		}()
		limiter := newConnectionRateLimiter(nil, defaultConnectionRateLimits)
		for {
			messageType, msg, err := ws.ReadMessage()
			if err != nil {
				var ne net.Error
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					logger.Infof(ctx, "App Normal disconnection: mac=%s, deviceType=%s, Error=%v", mac, deviceType, err)
					break
				}
				if errors.As(err, &ne) && ne.Temporary() {
					logger.Infof(ctx, "App Temporary network error. Continue reading.: mac=%s,deviceType=%s,Error=%v", mac, deviceType, err)
					continue
				}
				if errors.As(err, &ne) && ne.Timeout() {
					logger.Infof(ctx, "App Timeout disconnection: mac=%s, deviceType=%s", mac, deviceType)
					break
				}
				logger.Errorf(ctx, "App Abnormal disconnection: mac=%s, deviceType=%s, Error=%v", mac, deviceType, err)
				break
			}
			if client.ConnectionGeneration() != connectionGeneration {
				break
			}
			if !allowInbound(limiter, messageType, msg) {
				logger.Warningf(ctx, "App message rate limit exceeded: mac=%s", mac)
				break
			}
			client.SetLastTime(time.Now())
			readAppClientMessage(ctx, client, &messageType, &msg)
		}
	}
}

// Handle WebSocket connection requests from StackChan devices
func addStackChenClient(ctx context.Context, c *model.StackChanClient) {
	stackChanClientPool.Store(c.GetMac(), c)
	// The meeting-v1 Device credential is already the authoritative device
	// identity and pairing repository key. Do not route such a connection
	// through the legacy Device DAO: a self-hosted meeting server may
	// intentionally omit the unrelated legacy SQL database, and a failed
	// InsertIgnore previously made an otherwise successful WebSocket upgrade
	// report HTTP 500 and repeatedly evict the device.
	//
	// Keep the original persistence behavior for legacy RSA-authenticated
	// devices, or when an operator explicitly opts back in.
	if os.Getenv("STACKCHAN_DEVICE_CREDENTIALS") == "" ||
		booleanEnvironment("STACKCHAN_LEGACY_DEVICE_PERSISTENCE", false) {
		if _, err := service.CreateMacIfNotExists(ctx, c.GetMac()); err != nil {
			logger.Warningf(ctx, "Legacy device persistence failed for %s: %v", c.GetMac(), err)
		}
	}
}

// Handle WebSocket connection requests from App devices
func addAppClient(c *model.AppClient) {
	appClientMu.Lock()
	defer appClientMu.Unlock()

	val, _ := appClientPool.Load(c.GetMac())
	var clients []*model.AppClient
	if val != nil {
		clients = append(val.([]*model.AppClient), c)
	} else {
		clients = []*model.AppClient{c}
	}
	appClientPool.Store(c.GetMac(), clients)
}

// Get all App clients with specified MAC address
func getAppClients(mac string) []*model.AppClient {
	if val, ok := appClientPool.Load(mac); ok {
		return val.([]*model.AppClient)
	}
	return nil
}

// Get StackChan client with specified MAC address
func getStackChanClient(mac string) *model.StackChanClient {
	if val, ok := stackChanClientPool.Load(mac); ok {
		return val.(*model.StackChanClient)
	}
	return nil
}

func connectedDeviceCount() int {
	count := 0
	stackChanClientPool.Range(func(_, value any) bool {
		client, ok := value.(*model.StackChanClient)
		if ok && client != nil && client.GetConn() != nil {
			count++
		}
		return true
	})
	return count
}

// Parse custom binary protocol messages, return message type, data length, payload and success status
func parseBinaryMessage(ctx context.Context, msg *[]byte) (byte, int, []byte, bool) {
	msgType, payload, err := wsprotocol.ParseBinaryMessage(*msg)
	if err != nil {
		logger.Warningf(ctx, "Invalid binary message: %v; message not forwarded", err)
		return 0, 0, nil, false
	}
	return msgType, len(payload), payload, true
}

// Handle WebSocket messages from StackChan devices
func readStackChanMessage(ctx context.Context, client *model.StackChanClient, messageType *int, msg *[]byte) {
	if *messageType == websocket.BinaryMessage {
		msgType, _, payload, ok := parseBinaryMessage(ctx, msg)
		if !ok {
			return
		}
		if msgType == MeetingControl {
			handleDeviceMeetingControl(ctx, client, payload)
			return
		}
		if msgType == Opus && client.SupportsMeetingV1(client.ConnectionGeneration()) && meetingManager.Active(client.GetMac()) != nil {
			if err := meetingManager.OnAudio(ctx, client.GetMac(), payload); err != nil {
				request := wsprotocol.MeetingControl{}
				if frame, decodeErr := wsprotocol.DecodeMeetingAudio(payload); decodeErr == nil {
					request.SessionID = frame.SessionID.String()
				}
				notifyMeetingOwnerError(ctx, client.GetMac(), request, err)
			}
			return
		}
		switch msgType {
		case pong:
			break
		case ControlAvatar, ControlMotion, OnCamera, OffCamera:
			break
		case RefuseCall:
			// Reject call, remove and notify App client
			appClient := client.GetCallAppClient()
			if appClient != nil {
				appSendMessage(ctx, appClient, messageType, msg)
				client.SetCallAppClient(nil)
			}
			break
		case AgreeCall:
			// Accept call, add App client to subscription list
			appClient := client.GetCallAppClient()
			if appClient != nil {
				appSendMessage(ctx, appClient, messageType, msg)
				if client.AddCameraSubscriber(appClient) && len(client.GetCameraSubscriptionList()) == 1 {
					onMsg := createMessage(OnCamera, nil)
					onType := websocket.BinaryMessage
					stackChanSendMessage(ctx, client, &onType, onMsg)
				}
				if client.AddAudioSubscriber(appClient) && len(client.GetAudioSubscriptionList()) == 1 {
					onMsg := createMessage(OnAudio, nil)
					onType := websocket.BinaryMessage
					stackChanSendMessage(ctx, client, &onType, onMsg)
				}
			}
			break
		case HangupCall:
			// Hang up call, remove App client and update subscription list
			appClient := client.GetCallAppClient()
			if appClient != nil {
				appSendMessage(ctx, appClient, messageType, msg)
				// Remove the client from the subscription list
				_, cameraEmpty := client.RemoveCameraSubscriber(appClient)
				// If the subscription list is empty, notify to turn off the camera
				if cameraEmpty {
					offMsg := createMessage(OffCamera, nil)
					offType := websocket.BinaryMessage
					stackChanSendMessage(ctx, client, &offType, offMsg)
				}

				_, audioEmpty := client.RemoveAudioSubscriber(appClient)
				if audioEmpty {
					offMsg := createMessage(OffAudio, nil)
					offType := websocket.BinaryMessage
					stackChanSendMessage(ctx, client, &offType, offMsg)
				}
			}
			break
		case GetDeviceName:
			// Query device name
			name, err := service.GetDeviceName(ctx, client.GetMac())
			if err != nil {
				return
			}
			if name == "" {
				logger.Infof(ctx, "Queried device nickname is empty")
				return
			}
			newMsg := createStringMessage(GetDeviceName, name)
			stackChanSendMessage(ctx, client, messageType, newMsg)
			break
		case Opus:
			subscribers := client.GetAudioSubscriptionList()
			if len(subscribers) > 0 {
				var isAll = true
				for _, subClient := range client.GetAudioSubscriptionList() {
					if subClient.GetConn() != nil {
						isAll = false
					}
					appSendMessage(ctx, subClient, messageType, msg)
				}
				if isAll {
					msg = createMessage(OffAudio, nil)
					stackChanSendMessage(ctx, client, messageType, msg)
				}
			} else {
				msg = createMessage(OffAudio, nil)
				stackChanSendMessage(ctx, client, messageType, msg)
			}
			break
		case Jpeg:
			subscribers := client.GetCameraSubscriptionList()
			if len(subscribers) > 0 {
				var isAll = true
				for _, subClient := range subscribers {
					if subClient.GetConn() != nil {
						isAll = false
					}
					appSendMessage(ctx, subClient, messageType, msg)
				}
				if isAll {
					msg = createMessage(OffCamera, nil)
					stackChanSendMessage(ctx, client, messageType, msg)
				}
			} else {
				msg = createMessage(OffCamera, nil)
				stackChanSendMessage(ctx, client, messageType, msg)
			}
			break
		case GetAvatarPosture:
			appClients := getAppClients(client.GetMac())
			for _, appClient := range appClients {
				appSendMessage(ctx, appClient, messageType, msg)
			}
			break
		case AimedTakePhoto:
			appClient := client.GetAimedTakePhotoAppClient()
			if appClient != nil {
				appSendMessage(ctx, appClient, messageType, msg)
			}
			break
		default:
			logger.Infof(ctx, "Unknown binary msgType: %d", msgType)
			appClients := getAppClients(client.GetMac())
			if appClients != nil {
				for _, appClient := range appClients {
					appSendMessage(ctx, appClient, messageType, msg)
				}
			}
		}
	} else if *messageType == websocket.TextMessage {
		appClients := getAppClients(client.GetMac())
		if appClients != nil {
			for _, appClient := range appClients {
				appSendMessage(ctx, appClient, messageType, msg)
			}
		}
	} else if *messageType == websocket.PingMessage {
		logger.Info(ctx, "Received ping message from StackChan side")
	}
}

// Handle WebSocket messages from App clients
func readAppClientMessage(ctx context.Context, client *model.AppClient, messageType *int, msg *[]byte) {
	if *messageType == websocket.BinaryMessage {
		msgType, _, payload, ok := parseBinaryMessage(ctx, msg)
		if !ok {
			return
		}
		if msgType == MeetingControl {
			handleAppMeetingControl(ctx, client, payload)
			return
		}
		switch msgType {
		case pong:
			break
		case GetDeviceName:
			// Query device name
			name, err := service.GetDeviceName(ctx, client.GetMac())
			if err != nil {
				logger.Errorf(ctx, "Get device name failed: %v", err)
				return
			}
			if name == "" {
				logger.Infof(ctx, "Queried device nickname is empty")
				return
			}
			newMsg := createStringMessage(GetDeviceName, name)
			logger.Infof(ctx, "Device name found, returning: %s", name)
			appSendMessage(ctx, client, messageType, newMsg)
			break
		case UpdateDeviceName:
			stackChanClient := getStackChanClient(client.GetMac())
			if stackChanClient != nil {
				stackChanSendMessage(ctx, stackChanClient, messageType, msg)
			}
			appClients := getAppClients(client.GetMac())
			for _, appClient := range appClients {
				appSendMessage(ctx, appClient, messageType, msg)
			}
			break
		case Opus:
			if payload == nil || len(payload) < 12 {
				logger.Warningf(ctx, "Payload too short, cannot parse MAC address: %v", payload)
				return
			}
			macAddrBytes := payload[:12]
			data := payload[12:]
			macAddr := string(macAddrBytes)
			newMsg := createMessage(msgType, data)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				stackChanSendMessage(ctx, stackChanClient, messageType, newMsg)
			}
			break
		case Jpeg:
			if payload == nil || len(payload) < 12 {
				logger.Warningf(ctx, "Payload too short, cannot parse MAC address: %v", payload)
				return
			}
			macAddrBytes := payload[:12]
			data := payload[12:]
			macAddr := string(macAddrBytes)
			newMsg := createMessage(msgType, data)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				if stackChanClient.GetPhoneScreen() {
					stackChanSendMessage(ctx, stackChanClient, messageType, newMsg)
				}
			}
			break
		case ControlAvatar, ControlMotion:
			if payload == nil || len(payload) < 12 {
				logger.Warningf(ctx, "Payload too short, cannot parse MAC address: %v", payload)
				return
			}
			macAddrBytes := payload[:12]
			data := payload[12:]
			macAddr := string(macAddrBytes)
			newMsg := createMessage(msgType, data)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				stackChanSendMessage(ctx, stackChanClient, messageType, newMsg)
			} else {
				logger.Infof(ctx, "StackChan is currently offline")
			}
			break
		case TextMessage:
			if payload == nil || len(payload) < 12 {
				logger.Warningf(ctx, "Payload too short, cannot parse MAC address: %v", payload)
				return
			}
			macAddr := string(payload[:12])
			data := payload[12:]
			newMsg := createMessage(msgType, data)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				stackChanSendMessage(ctx, stackChanClient, messageType, newMsg)
			}
			appClients := getAppClients(macAddr)
			if appClients != nil {
				for _, appClient := range appClients {
					appSendMessage(ctx, appClient, messageType, newMsg)
				}
			}
			break
		case RequestCall:
			// Request call
			if payload == nil || len(payload) < 12 {
				logger.Warningf(ctx, "Payload too short, cannot parse MAC address: %v", payload)
				return
			}
			macAddr := string(payload[:12])
			data := payload[12:]
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				if stackChanClient.GetCallAppClient() == nil || stackChanClient.GetCallAppClient() == client {
					stackChanClient.SetCallAppClient(client)
					newMsg := createMessage(msgType, data)
					stackChanSendMessage(ctx, stackChanClient, messageType, newMsg)
				} else {
					// Notify App that the other side is already in a call
					newMsg := createStringMessage(inCall, "The other party is currently in a call")
					appSendMessage(ctx, client, messageType, newMsg)
				}
			}
			break
		case HangupCall:
			stackChanClientPool.Range(func(_, value any) bool {
				stackChanClient := value.(*model.StackChanClient)
				if stackChanClient.GetCallAppClient() == client {
					// Found corresponding call
					stackChanClient.SetCallAppClient(nil)
					stackChanSendMessage(ctx, stackChanClient, messageType, msg)

					removedCamera, cameraEmpty := stackChanClient.RemoveCameraSubscriber(client)
					if removedCamera && cameraEmpty {
						offMsg := createMessage(OffCamera, nil)
						offType := websocket.BinaryMessage
						stackChanSendMessage(ctx, stackChanClient, &offType, offMsg)
					}

					removedAudio, audioEmpty := stackChanClient.RemoveAudioSubscriber(client)
					if removedAudio && audioEmpty {
						offMsg := createMessage(OffAudio, nil)
						offType := websocket.BinaryMessage
						stackChanSendMessage(ctx, stackChanClient, &offType, offMsg)
					}

					return false
				}
				return true
			})
			break
		case OnAudio:
			macAddr := string(payload)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				if stackChanClient.AddAudioSubscriber(client) {
					stackChanSendMessage(ctx, stackChanClient, messageType, msg)
				}
			}
			break
		case OffAudio:
			macAddr := string(payload)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				removed, empty := stackChanClient.RemoveAudioSubscriber(client)
				if removed && empty {
					stackChanSendMessage(ctx, stackChanClient, messageType, msg)
				}
			}
			break
		case OnCamera:
			macAddr := string(payload)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				if stackChanClient.AddCameraSubscriber(client) {
					stackChanSendMessage(ctx, stackChanClient, messageType, msg)
				}
			}
			break
		case OffCamera:
			macAddr := string(payload)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				removed, empty := stackChanClient.RemoveCameraSubscriber(client)
				if removed && empty {
					stackChanSendMessage(ctx, stackChanClient, messageType, msg)
				}
			}
			break
		case OnPhoneScreen:
			// Show phone screen
			macAddr := string(payload)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				if stackChanClient.GetPhoneScreen() == false {
					stackChanClient.SetPhoneScreen(true)
					stackChanSendMessage(ctx, stackChanClient, messageType, msg)
				}
			}
			break
		case OffPhoneScreen:
			// Hide phone screen
			macAddr := string(payload)
			stackChanClient := getStackChanClient(macAddr)
			if stackChanClient != nil {
				if stackChanClient.GetPhoneScreen() == true {
					stackChanClient.SetPhoneScreen(false)
					stackChanSendMessage(ctx, stackChanClient, messageType, msg)
				}
			}
			break
		case Dance:
			// Dance message
			stackChanClient := getStackChanClient(client.GetMac())
			if stackChanClient != nil {
				stackChanSendMessage(ctx, stackChanClient, messageType, msg)
			}
			break
		case GetAvatarPosture:
			stackChanClient := getStackChanClient(client.GetMac())
			if stackChanClient != nil {
				stackChanSendMessage(ctx, stackChanClient, messageType, msg)
			}
		case AimedTakePhoto:
			stackChanClient := getStackChanClient(client.GetMac())
			if stackChanClient != nil {
				stackChanClient.SetAimedTakePhotoAppClient(client)
				stackChanSendMessage(ctx, stackChanClient, messageType, msg)
			}
			break
		default:
			logger.Infof(ctx, "Unknown binary msgType: %d", msgType)
			stackChanClient := getStackChanClient(client.GetMac())
			if stackChanClient != nil {
				stackChanSendMessage(ctx, stackChanClient, messageType, msg)
			}
		}
	} else if *messageType == websocket.TextMessage {
		// Directly forward other message types
		stackChanClient := getStackChanClient(client.GetMac())
		if stackChanClient != nil {
			stackChanSendMessage(ctx, stackChanClient, messageType, msg)
		}
	} else if *messageType == websocket.PingMessage {
		logger.Info(ctx, "Received ping message from App side")
	}
}

// Send WebSocket messages to App clients
func appSendMessage(ctx context.Context, client *model.AppClient, messageType *int, msg *[]byte) model.SendResult {
	if client == nil {
		return model.SendInvalid
	}
	return appSendMessageForGeneration(ctx, client, client.ConnectionGeneration(), messageType, msg)
}

func appSendMessageForGeneration(ctx context.Context, client *model.AppClient, generation uint64, messageType *int, msg *[]byte) model.SendResult {
	if client == nil || messageType == nil || msg == nil {
		return model.SendInvalid
	}
	result := client.TrySendForGeneration(generation, &model.WsSendMsg{MsgType: *messageType, Data: *msg})
	meetingmetrics.Default.SetSendQueueDepth(int64(len(client.SendChan())))
	if result != model.SendEnqueued {
		logger.Warningf(ctx, "App client enqueue failed: result=%d", result)
	}
	return result
}

// Send WebSocket messages to StackChan devices
func stackChanSendMessage(ctx context.Context, client *model.StackChanClient, messageType *int, msg *[]byte) model.SendResult {
	if client == nil || messageType == nil || msg == nil {
		return model.SendInvalid
	}
	generation := client.ConnectionGeneration()
	result := client.TrySendForGeneration(generation, &model.WsSendMsg{MsgType: *messageType, Data: *msg})
	meetingmetrics.Default.SetSendQueueDepth(int64(len(client.SendChan())))
	if result != model.SendEnqueued {
		logger.Warningf(ctx, "StackChan client enqueue failed: result=%d", result)
	}
	return result
}

// SendAppMessage Send WebSocket messages to App clients
func SendAppMessage(ctx context.Context, mac string, messageType *int, msg *[]byte, supportOfflineMode *bool) []model.SendResult {
	clients := getAppClients(mac)
	results := make([]model.SendResult, 0, len(clients))
	if clients != nil {
		for _, client := range clients {
			results = append(results, appSendMessage(ctx, client, messageType, msg))
		}
	}
	return results
}

// SendStackChanMessage Send WebSocket messages to StackChan devices
func SendStackChanMessage(ctx context.Context, mac string, messageType *int, msg *[]byte, supportOfflineMode *bool) model.SendResult {
	stackChanClient := getStackChanClient(mac)
	if stackChanClient != nil {
		return stackChanSendMessage(ctx, stackChanClient, messageType, msg)
	}
	return model.SendUnavailable
}

// Encapsulate binary messages for custom protocol (type + data length + data)
func createMessage(msgType byte, data []byte) *[]byte {
	var dataLen int
	if data != nil {
		dataLen = len(data)
	} else {
		dataLen = 0
	}
	msg := make([]byte, 1+4+dataLen)
	msg[0] = msgType
	binary.BigEndian.PutUint32(msg[1:5], uint32(dataLen))
	if dataLen > 0 {
		copy(msg[5:], data)
	}
	return &msg
}

// Encapsulate binary messages for custom protocol (type + data length + string data)
func createStringMessage(msgType byte, data string) *[]byte {
	return createMessage(msgType, []byte(data))
}
