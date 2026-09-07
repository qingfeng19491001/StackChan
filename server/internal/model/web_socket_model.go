/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package model

import (
	"context"
	"sync"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gorilla/websocket"
)

type WsSendMsg struct {
	MsgType    int
	Data       []byte
	Generation uint64
}

const ClientSendQueueCapacity = 100

type SendResult uint8

const (
	SendEnqueued SendResult = iota
	SendQueueFull
	SendClosed
	SendInvalid
	SendUnavailable
)

type AppClient struct {
	mac                     string
	userID                  string
	conn                    *websocket.Conn
	connGeneration          uint64
	meetingV1Generation     uint64
	meetingTicketMAC        string
	meetingTicketUser       string
	meetingTicketDevice     string
	meetingTicketGeneration uint64
	mu                      sync.RWMutex
	deviceId                string
	lastTime                time.Time

	sendChan  chan *WsSendMsg
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	writerWG  sync.WaitGroup
}

type StackChanClient struct {
	mac                     string
	conn                    *websocket.Conn
	connGeneration          uint64
	meetingV1Generation     uint64
	mu                      sync.RWMutex
	cameraSubscriptionList  []*AppClient
	audioSubscriptionList   []*AppClient
	callAppClient           *AppClient
	aimedTakePhotoAppClient *AppClient
	phoneScreen             bool
	lastTime                time.Time

	sendChan  chan *WsSendMsg
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	writerWG  sync.WaitGroup
}

// NewAppClient creates and initializes an AppClient
func NewAppClient(mac string, conn *websocket.Conn, deviceId string) *AppClient {
	ctx, cancel := context.WithCancel(context.Background())
	client := &AppClient{
		mac:      mac,
		conn:     conn,
		deviceId: deviceId,
		lastTime: time.Now(),
		sendChan: make(chan *WsSendMsg, ClientSendQueueCapacity),
		ctx:      ctx,
		cancel:   cancel,
	}
	if conn != nil {
		client.connGeneration = 1
	}
	client.StartWriterCoroutine()
	return client
}

// NewStackChanClient creates and initializes a StackChanClient
func NewStackChanClient(mac string, conn *websocket.Conn, cameraSubscriptionList []*AppClient, callAppClient *AppClient, phoneScreen bool) *StackChanClient {
	ctx, cancel := context.WithCancel(context.Background())
	client := &StackChanClient{
		mac:                    mac,
		conn:                   conn,
		cameraSubscriptionList: cameraSubscriptionList,
		callAppClient:          callAppClient,
		phoneScreen:            phoneScreen,
		lastTime:               time.Now(),
		sendChan:               make(chan *WsSendMsg, ClientSendQueueCapacity),
		ctx:                    ctx,
		cancel:                 cancel,
	}
	if conn != nil {
		client.connGeneration = 1
	}
	client.StartWriterCoroutine()
	return client
}

// StartWriterCoroutine AppClient Start message sending coroutine
func (a *AppClient) StartWriterCoroutine() {
	a.writerWG.Add(1)
	go func() {
		defer a.writerWG.Done()
		defer func() {
			if r := recover(); r != nil {
				g.Log().Errorf(context.Background(), "AppClient writer coroutine panic: %v", r)
			}
		}()

		for {
			select {
			case <-a.ctx.Done():
				return
			case msg, ok := <-a.sendChan:
				if !ok { // Channel closed
					return
				}
				if msg == nil {
					continue
				}
				a.mu.RLock()
				conn, generation := a.conn, a.connGeneration
				a.mu.RUnlock()
				if conn == nil || (msg.Generation != 0 && msg.Generation != generation) {
					continue
				}
				if err := conn.WriteMessage(msg.MsgType, msg.Data); err != nil {
					g.Log().Errorf(context.Background(), "AppClient send message error: %v", err)
					// Make the owning read loop observe the write-side failure. Its
					// generation-aware deferred cleanup then interrupts any meeting
					// rather than leaving a dead owner attached to a live session.
					_ = conn.Close()
				}
			}
		}
	}()
}

// StartWriterCoroutine StackChanClient Start message sending coroutine
func (s *StackChanClient) StartWriterCoroutine() {
	s.writerWG.Add(1)
	go func() {
		defer s.writerWG.Done()
		defer func() {
			if r := recover(); r != nil {
				g.Log().Errorf(context.Background(), "StackChan writer coroutine panic: %v", r)
			}
		}()
		for {
			select {
			case <-s.ctx.Done():
				return
			case msg, ok := <-s.sendChan:
				if !ok {
					return
				}
				if msg == nil {
					continue
				}
				s.mu.RLock()
				conn, generation := s.conn, s.connGeneration
				s.mu.RUnlock()
				if conn == nil || (msg.Generation != 0 && msg.Generation != generation) {
					continue
				}
				if err := conn.WriteMessage(msg.MsgType, msg.Data); err != nil {
					g.Log().Errorf(context.Background(), "StackChan writer coroutine send message error: %v", err)
					// See AppClient: closing couples write failure to the existing
					// read-loop lifecycle and meeting interruption path.
					_ = conn.Close()
				}
			}
		}
	}()
}

func (a *AppClient) CloseWriterCoroutine() {
	a.closeOnce.Do(func() {
		if a.cancel != nil {
			a.cancel()
		}
	})
	a.writerWG.Wait()
}

func (s *StackChanClient) CloseWriterCoroutine() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	s.writerWG.Wait()
}

func trySend(ctx context.Context, sendChan chan *WsSendMsg, msg *WsSendMsg) SendResult {
	if msg == nil || sendChan == nil {
		return SendInvalid
	}
	if ctx == nil || ctx.Err() != nil {
		return SendClosed
	}
	copyOfMessage := &WsSendMsg{MsgType: msg.MsgType, Data: append([]byte(nil), msg.Data...), Generation: msg.Generation}
	select {
	case <-ctx.Done():
		return SendClosed
	case sendChan <- copyOfMessage:
		return SendEnqueued
	default:
		return SendQueueFull
	}
}

func (a *AppClient) TrySend(msg *WsSendMsg) SendResult {
	return a.TrySendForGeneration(a.ConnectionGeneration(), msg)
}

func (s *StackChanClient) TrySend(msg *WsSendMsg) SendResult {
	return s.TrySendForGeneration(s.ConnectionGeneration(), msg)
}

// TrySendForGeneration binds a queued message to one WebSocket attachment.
// A stale handler must never place a message that can be written to a later
// replacement connection using the same logical client object.
func (a *AppClient) TrySendForGeneration(generation uint64, msg *WsSendMsg) SendResult {
	if msg == nil {
		return SendInvalid
	}
	a.mu.RLock()
	current, connected := a.connGeneration, a.conn != nil
	a.mu.RUnlock()
	if generation == 0 || generation != current || !connected {
		return SendUnavailable
	}
	copy := *msg
	copy.Generation = generation
	return trySend(a.ctx, a.sendChan, &copy)
}

func (s *StackChanClient) TrySendForGeneration(generation uint64, msg *WsSendMsg) SendResult {
	if msg == nil {
		return SendInvalid
	}
	s.mu.RLock()
	current, connected := s.connGeneration, s.conn != nil
	s.mu.RUnlock()
	if generation == 0 || generation != current || !connected {
		return SendUnavailable
	}
	copy := *msg
	copy.Generation = generation
	return trySend(s.ctx, s.sendChan, &copy)
}

func (a *AppClient) SendChan() chan *WsSendMsg {
	return a.sendChan
}

func (s *StackChanClient) SendChan() chan *WsSendMsg {
	return s.sendChan
}

func (a *AppClient) SetMac(mac string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mac = mac
}

func (a *AppClient) GetMac() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.mac
}

func (a *AppClient) SetUserID(userID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.userID = userID
}

func (a *AppClient) GetUserID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.userID
}

func (a *AppClient) GetConn() *websocket.Conn {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.conn
}

func (a *AppClient) ConnectionSnapshot() (*websocket.Conn, uint64) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.conn, a.connGeneration
}

func (a *AppClient) SetConn(conn *websocket.Conn) {
	a.ReplaceConnection(conn)
}

func (a *AppClient) ReplaceConnection(conn *websocket.Conn) uint64 {
	_, generation := a.SwapConnection(conn)
	return generation
}

func (a *AppClient) SwapConnection(conn *websocket.Conn) (*websocket.Conn, uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	previous := a.conn
	a.connGeneration++
	a.conn = conn
	a.meetingV1Generation = 0
	a.meetingTicketMAC = ""
	a.meetingTicketUser = ""
	a.meetingTicketDevice = ""
	a.meetingTicketGeneration = 0
	return previous, a.connGeneration
}

func (a *AppClient) ConnectionGeneration() uint64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.connGeneration
}

func (a *AppClient) ClearConnection(generation uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if generation == 0 || a.connGeneration != generation {
		return false
	}
	a.conn = nil
	return true
}

func (a *AppClient) SelectMeetingV1(generation uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if generation == 0 || generation != a.connGeneration || a.conn == nil {
		return false
	}
	a.meetingV1Generation = generation
	return true
}

func (a *AppClient) SupportsMeetingV1(generation uint64) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return generation != 0 && generation == a.connGeneration && generation == a.meetingV1Generation
}

// AuthorizeMeetingTicket binds meeting-v1 capability to the single-use ticket
// consumed for this connection generation. Legacy App credentials never set it.
func (a *AppClient) AuthorizeMeetingTicket(mac string, generation uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if generation == 0 || generation != a.connGeneration || a.conn == nil || a.mac != mac {
		return false
	}
	a.meetingTicketMAC = mac
	a.meetingTicketUser = a.userID
	a.meetingTicketDevice = a.deviceId
	a.meetingTicketGeneration = generation
	return true
}

func (a *AppClient) MeetingTicketMAC(generation uint64) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if generation == 0 || generation != a.connGeneration || a.meetingTicketMAC == "" {
		return ""
	}
	return a.meetingTicketMAC
}

// SetMeetingAuthorization is a testable/runtime seam for an authorized
// ticket attachment. Production Handler uses AuthorizeMeetingTicket.
func (a *AppClient) SetMeetingAuthorization(userID, deviceID string, generation uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.userID, a.deviceId = userID, deviceID
	a.meetingTicketMAC, a.meetingTicketUser = a.mac, userID
	a.meetingTicketDevice, a.meetingTicketGeneration = deviceID, generation
}

func (a *AppClient) MeetingAuthorization(generation uint64) (string, string, string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if generation == 0 || generation != a.meetingTicketGeneration || a.meetingTicketMAC == "" {
		return "", "", "", false
	}
	return a.meetingTicketUser, a.meetingTicketDevice, a.meetingTicketMAC, true
}

func (a *AppClient) SetDeviceId(deviceId string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deviceId = deviceId
}

func (a *AppClient) GetDeviceId() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.deviceId
}

func (a *AppClient) SetLastTime(lastTime time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastTime = lastTime
}

func (a *AppClient) GetLastTime() time.Time {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastTime
}

func (s *StackChanClient) SetMac(mac string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mac = mac
}

func (s *StackChanClient) GetMac() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mac
}

func (s *StackChanClient) GetConn() *websocket.Conn {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn
}

func (s *StackChanClient) ConnectionSnapshot() (*websocket.Conn, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn, s.connGeneration
}

func (s *StackChanClient) SetConn(conn *websocket.Conn) {
	s.ReplaceConnection(conn)
}

func (s *StackChanClient) ReplaceConnection(conn *websocket.Conn) uint64 {
	_, generation := s.SwapConnection(conn)
	return generation
}

func (s *StackChanClient) SwapConnection(conn *websocket.Conn) (*websocket.Conn, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.conn
	s.connGeneration++
	s.conn = conn
	return previous, s.connGeneration
}

func (s *StackChanClient) ConnectionGeneration() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connGeneration
}

func (s *StackChanClient) ClearConnection(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == 0 || s.connGeneration != generation {
		return false
	}
	s.conn = nil
	return true
}

func (s *StackChanClient) SelectMeetingV1(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation == 0 || generation != s.connGeneration || s.conn == nil {
		return false
	}
	s.meetingV1Generation = generation
	return true
}

func (s *StackChanClient) SupportsMeetingV1(generation uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return generation != 0 && generation == s.connGeneration && generation == s.meetingV1Generation
}

func (s *StackChanClient) SetCameraSubscriptionList(cameraSubscriptionList []*AppClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cameraSubscriptionList = cameraSubscriptionList
}

func (s *StackChanClient) AppendCameraSubscriptionList(appClient *AppClient) {
	s.AddCameraSubscriber(appClient)
}

func (s *StackChanClient) AddCameraSubscriber(appClient *AppClient) bool {
	if appClient == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.cameraSubscriptionList {
		if existing == appClient {
			return false
		}
	}
	s.cameraSubscriptionList = append(s.cameraSubscriptionList, appClient)
	return true
}

func (s *StackChanClient) RemoveCameraSubscriber(appClient *AppClient) (removed bool, empty bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.cameraSubscriptionList[:0]
	for _, existing := range s.cameraSubscriptionList {
		if existing == appClient {
			removed = true
			continue
		}
		filtered = append(filtered, existing)
	}
	s.cameraSubscriptionList = filtered
	return removed, len(filtered) == 0
}

func (s *StackChanClient) GetCameraSubscriptionList() []*AppClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AppClient, len(s.cameraSubscriptionList))
	copy(out, s.cameraSubscriptionList)
	return out
}

func (s *StackChanClient) SetAudioSubscriptionList(audioSubscriptionList []*AppClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioSubscriptionList = audioSubscriptionList
}

func (s *StackChanClient) AddAudioSubscriber(appClient *AppClient) bool {
	if appClient == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.audioSubscriptionList {
		if existing == appClient {
			return false
		}
	}
	s.audioSubscriptionList = append(s.audioSubscriptionList, appClient)
	return true
}

func (s *StackChanClient) RemoveAudioSubscriber(appClient *AppClient) (removed bool, empty bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.audioSubscriptionList[:0]
	for _, existing := range s.audioSubscriptionList {
		if existing == appClient {
			removed = true
			continue
		}
		filtered = append(filtered, existing)
	}
	s.audioSubscriptionList = filtered
	return removed, len(filtered) == 0
}

func (s *StackChanClient) GetAudioSubscriptionList() []*AppClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AppClient, len(s.audioSubscriptionList))
	copy(out, s.audioSubscriptionList)
	return out
}

func (s *StackChanClient) SetCallAppClient(client *AppClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callAppClient = client
}

func (s *StackChanClient) GetCallAppClient() *AppClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.callAppClient
}

func (s *StackChanClient) GetAimedTakePhotoAppClient() *AppClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.aimedTakePhotoAppClient
}

func (s *StackChanClient) SetAimedTakePhotoAppClient(aimedTakePhotoAppClient *AppClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aimedTakePhotoAppClient = aimedTakePhotoAppClient
}

func (s *StackChanClient) GetPhoneScreen() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phoneScreen
}

func (s *StackChanClient) SetPhoneScreen(phoneScreen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phoneScreen = phoneScreen
}

func (s *StackChanClient) GetLastTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastTime
}

func (s *StackChanClient) SetLastTime(lastTime time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastTime = lastTime
}
