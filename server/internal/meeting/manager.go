package meeting

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"stackChan/internal/meetingaudit"
	"stackChan/internal/meetingmetrics"
	"stackChan/internal/pairing"
	wsprotocol "stackChan/internal/web_socket/protocol"
)

var (
	ErrSessionBusy      = errors.New("SESSION_BUSY")
	ErrUnauthorized     = errors.New("UNAUTHORIZED")
	ErrDeviceOffline    = errors.New("DEVICE_OFFLINE")
	ErrSessionNotFound  = errors.New("session not found")
	ErrInvalidEvent     = errors.New("invalid device event")
	ErrAudioGap         = errors.New("AUDIO_GAP")
	ErrStartTimeout     = errors.New("START_TIMEOUT")
	ErrOwnerOffline     = errors.New("owner offline")
	ErrReattachDisabled = errors.New("REATTACH_DISABLED")
)

const startOfferTTL = 30 * time.Second
const audioReplayWindow = 5 * time.Second
const audioReplayMaxBytes = 128 * 1024
const ownerReattachWindow = 5 * time.Second

type Owner struct {
	NodeID     string
	MAC        string
	UserID     string
	DeviceID   string
	Generation uint64
}
type StartCommand struct {
	Owner                     Owner
	MAC, SessionID, CommandID string
}
type StopCommand struct {
	Owner                             Owner
	MAC, SessionID, CommandID, Reason string
}
type DeviceStartRequest struct {
	MAC, SessionID, CommandID string
}
type AcceptStartCommand struct {
	Owner                     Owner
	MAC, SessionID, CommandID string
}
type DeviceStopRequest struct {
	MAC, SessionID, CommandID, Reason string
}
type ReattachCommand struct {
	Owner                     Owner
	MAC, SessionID, CommandID string
	LastReceivedSequence      *uint32
}
type StartResult struct{ SessionID, State string }
type StopResult struct{ SessionID, State string }
type Event struct {
	Action, SessionID, CommandID, Reason, Code string
	FirstSequence, LastSequence                *uint32
}
type ControlMessage struct {
	Action, SessionID, CommandID, MAC, Reason, Code string
	FirstSequence, LastSequence                     *uint32
	ExpiresAt                                       int64
}
type ControlRoute struct {
	MAC     string
	Owner   Owner
	Message ControlMessage
}

type Transport interface {
	SendDevice(context.Context, string, ControlMessage) error
	SendBoundApps(context.Context, string, ControlMessage) error
	SendOwner(context.Context, Owner, ControlMessage) error
	SendOwnerAudio(context.Context, Owner, []byte) error
}

type SessionStore interface {
	Claim(context.Context, RedisSessionRecord) error
	PromoteOffer(context.Context, RedisSessionRecord) (bool, error)
	ReleaseOffer(context.Context, string, string, string) (bool, error)
	Save(context.Context, RedisSessionRecord) error
	Load(context.Context, string) (*RedisSessionRecord, error)
	AppendReplay(context.Context, string, string, RedisReplayFrame) error
	LoadReplay(context.Context, string, string, time.Time) ([]RedisReplayFrame, error)
	SaveCommand(context.Context, RedisCommandRecord) error
	LoadCommand(context.Context, string, string, string, string) (*RedisCommandRecord, error)
	Release(context.Context, string, string) (bool, error)
}

type pendingStart struct {
	MAC, SessionID, CommandID string
	ExpiresAt                 time.Time
}

type Session struct {
	MAC, SessionID, StartCommandID, StopCommandID, State string
	Owner                                                Owner
	NextSequence                                         uint32
	AudioReceived                                        bool
	ExpiresAt                                            int64
	audioBuffer                                          []bufferedAudio
	audioBufferBytes                                     int
	recordingStartedAt                                   time.Time
	firstFrameObserved                                   bool
}

type bufferedAudio struct {
	sequence   uint32
	payload    []byte
	receivedAt time.Time
}

type MemoryManager struct {
	mu             sync.Mutex
	transport      Transport
	store          SessionStore
	activeByMAC    map[string]*Session
	starts         map[string]StartResult
	stops          map[string]StopResult
	pendingByMAC   map[string]*pendingStart
	expiredStarts  map[string]struct{}
	pendingOwners  map[Owner]struct{}
	reattaches     map[string]struct{}
	now            func() time.Time
	scheduleDelay  time.Duration
	enableReattach bool
}

type ManagerOptions struct {
	EnableReattach bool
}

func NewMemoryManager(transport Transport) *MemoryManager {
	return newMemoryManagerWithOptions(transport, nil, time.Now, startOfferTTL, ManagerOptions{})
}

func newMemoryManager(transport Transport, now func() time.Time, scheduleDelay time.Duration) *MemoryManager {
	return newMemoryManagerWithOptions(transport, nil, now, scheduleDelay, ManagerOptions{})
}

func NewMemoryManagerWithStore(transport Transport, store SessionStore) *MemoryManager {
	return newMemoryManagerWithOptions(transport, store, time.Now, startOfferTTL, ManagerOptions{})
}

func newMemoryManagerWithStore(transport Transport, store SessionStore, now func() time.Time, scheduleDelay time.Duration) *MemoryManager {
	return newMemoryManagerWithOptions(transport, store, now, scheduleDelay, ManagerOptions{})
}

func NewMemoryManagerWithOptions(transport Transport, options ManagerOptions) *MemoryManager {
	return newMemoryManagerWithOptions(transport, nil, time.Now, startOfferTTL, options)
}

func NewMemoryManagerWithOptionsAndStore(transport Transport, store SessionStore, options ManagerOptions) *MemoryManager {
	return newMemoryManagerWithOptions(transport, store, time.Now, startOfferTTL, options)
}

func newMemoryManagerWithOptions(transport Transport, store SessionStore, now func() time.Time, scheduleDelay time.Duration, options ManagerOptions) *MemoryManager {
	if now == nil {
		now = time.Now
	}
	return &MemoryManager{
		transport: transport, store: store, activeByMAC: make(map[string]*Session),
		starts: make(map[string]StartResult), stops: make(map[string]StopResult),
		pendingByMAC: make(map[string]*pendingStart), expiredStarts: make(map[string]struct{}), pendingOwners: make(map[Owner]struct{}),
		reattaches: make(map[string]struct{}), now: now, scheduleDelay: scheduleDelay,
		enableReattach: options.EnableReattach,
	}
}

func sessionRecord(session *Session) RedisSessionRecord {
	return RedisSessionRecord{
		MAC: session.MAC, SessionID: session.SessionID,
		StartCommandID: session.StartCommandID, StopCommandID: session.StopCommandID,
		State: session.State, Owner: session.Owner, NextSequence: session.NextSequence,
		AudioReceived: session.AudioReceived, ExpiresAt: session.ExpiresAt,
	}
}

// sessionFromRecord deliberately restores only durable state.  The short
// replay buffer is loaded separately because it has a much shorter TTL than
// the control-plane session record.
func sessionFromRecord(record *RedisSessionRecord) *Session {
	if record == nil {
		return nil
	}
	return &Session{
		MAC: record.MAC, SessionID: record.SessionID,
		StartCommandID: record.StartCommandID, StopCommandID: record.StopCommandID,
		State: record.State, Owner: record.Owner, NextSequence: record.NextSequence,
		AudioReceived: record.AudioReceived, ExpiresAt: record.ExpiresAt,
	}
}

// refreshSessionLocked makes Redis the authority for control-plane state when
// multiple server nodes are enabled. Callers hold m.mu. Audio frames refresh
// only on a local miss so the 60 ms data path remains local.
func (m *MemoryManager) refreshSessionLocked(ctx context.Context, mac string) (*Session, error) {
	if m.store == nil {
		return m.activeByMAC[mac], nil
	}
	record, err := m.store.Load(ctx, mac)
	if err != nil {
		return nil, err
	}
	if record == nil {
		delete(m.activeByMAC, mac)
		return nil, nil
	}
	current := m.activeByMAC[mac]
	refreshed := sessionFromRecord(record)
	if refreshed.State == "offered" {
		delete(m.activeByMAC, mac)
		return refreshed, nil
	}
	if current != nil && current.SessionID == refreshed.SessionID {
		// Preserve replay data held by the device-owning node.
		refreshed.audioBuffer = current.audioBuffer
		refreshed.audioBufferBytes = current.audioBufferBytes
		refreshed.recordingStartedAt = current.recordingStartedAt
		refreshed.firstFrameObserved = current.firstFrameObserved
	}
	m.activeByMAC[mac] = refreshed
	return refreshed, nil
}

func (m *MemoryManager) ensureSessionLocked(ctx context.Context, mac string) (*Session, error) {
	if session := m.activeByMAC[mac]; session != nil {
		return session, nil
	}
	return m.refreshSessionLocked(ctx, mac)
}

func (m *MemoryManager) claimSession(ctx context.Context, session *Session) (*RedisSessionRecord, bool, error) {
	if m.store == nil {
		return nil, true, nil
	}
	err := m.store.Claim(ctx, sessionRecord(session))
	if !errors.Is(err, ErrSessionBusy) {
		return nil, err == nil, err
	}
	existing, loadErr := m.store.Load(ctx, session.MAC)
	if loadErr != nil {
		return nil, false, loadErr
	}
	if existing != nil && existing.SessionID == session.SessionID &&
		existing.StartCommandID == session.StartCommandID && existing.Owner == session.Owner {
		return existing, false, nil
	}
	return nil, false, ErrSessionBusy
}

func (m *MemoryManager) saveSession(ctx context.Context, session *Session) error {
	if m.store == nil {
		return nil
	}
	return m.store.Save(ctx, sessionRecord(session))
}

func (m *MemoryManager) releaseSession(ctx context.Context, session *Session) error {
	if m.store == nil {
		return nil
	}
	_, err := m.store.Release(ctx, session.MAC, session.SessionID)
	return err
}

func (m *MemoryManager) releaseOffer(ctx context.Context, pending *pendingStart) (bool, error) {
	if m.store == nil || pending == nil {
		return true, nil
	}
	return m.store.ReleaseOffer(ctx, pending.MAC, pending.SessionID, pending.CommandID)
}

func (m *MemoryManager) loadCommand(ctx context.Context, action, mac, sessionID, commandID string) (*RedisCommandRecord, error) {
	if m.store == nil {
		return nil, nil
	}
	return m.store.LoadCommand(ctx, action, mac, sessionID, commandID)
}

func (m *MemoryManager) saveCommand(ctx context.Context, action string, session *Session, commandID, state string) error {
	if m.store == nil {
		return nil
	}
	return m.store.SaveCommand(ctx, RedisCommandRecord{
		Action: action, MAC: session.MAC, SessionID: session.SessionID,
		CommandID: commandID, Owner: session.Owner, State: state,
	})
}

func key(action, sessionID, commandID string) string {
	return action + "|" + sessionID + "|" + commandID
}

func validateIdentity(mac, sessionID, commandID string) (string, error) {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return "", err
	}
	if _, err := uuid.Parse(sessionID); err != nil {
		return "", fmt.Errorf("invalid sessionId: %w", err)
	}
	if _, err := uuid.Parse(commandID); err != nil {
		return "", fmt.Errorf("invalid commandId: %w", err)
	}
	return mac, nil
}

func (m *MemoryManager) RequestStart(ctx context.Context, request DeviceStartRequest) error {
	mac, err := validateIdentity(request.MAC, request.SessionID, request.CommandID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if m.activeByMAC[mac] != nil || m.pendingByMAC[mac] != nil {
		m.mu.Unlock()
		return ErrSessionBusy
	}
	expiresAt := m.now().Add(startOfferTTL)
	pending := &pendingStart{MAC: mac, SessionID: request.SessionID, CommandID: request.CommandID, ExpiresAt: expiresAt}
	offer := &Session{
		MAC: mac, SessionID: request.SessionID, StartCommandID: request.CommandID,
		State: "offered", ExpiresAt: expiresAt.Unix(),
	}
	existing, claimed, claimErr := m.claimSession(ctx, offer)
	if claimErr != nil {
		m.mu.Unlock()
		return claimErr
	}
	if !claimed {
		if existing == nil || existing.State != "offered" || existing.ExpiresAt == 0 {
			m.mu.Unlock()
			return ErrSessionBusy
		}
		pending.ExpiresAt = time.Unix(existing.ExpiresAt, 0)
	}
	m.pendingByMAC[mac] = pending
	m.mu.Unlock()

	message := ControlMessage{Action: "meeting.start-offered", SessionID: request.SessionID, CommandID: request.CommandID, MAC: mac, ExpiresAt: expiresAt.Unix()}
	if err := m.transport.SendBoundApps(ctx, mac, message); err != nil {
		m.mu.Lock()
		if m.pendingByMAC[mac] == pending {
			delete(m.pendingByMAC, mac)
		}
		m.mu.Unlock()
		if claimed {
			_, _ = m.releaseOffer(ctx, pending)
		}
		return err
	}
	time.AfterFunc(m.scheduleDelay, func() { m.ExpirePending(context.Background()) })
	return nil
}

func (m *MemoryManager) AcceptRequestedStart(ctx context.Context, command AcceptStartCommand) (StartResult, error) {
	mac, err := validateIdentity(command.MAC, command.SessionID, command.CommandID)
	if err != nil {
		return StartResult{}, err
	}
	if command.Owner.UserID == "" || command.Owner.DeviceID == "" {
		return StartResult{}, ErrUnauthorized
	}
	idempotencyKey := key("accept", command.SessionID, command.CommandID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if result, ok := m.starts[idempotencyKey]; ok {
		active := m.activeByMAC[mac]
		if active != nil && active.Owner == command.Owner {
			return result, nil
		}
		return StartResult{}, ErrSessionBusy
	}
	if previous, loadErr := m.loadCommand(ctx, "accept", mac, command.SessionID, command.CommandID); loadErr != nil {
		return StartResult{}, loadErr
	} else if previous != nil {
		if previous.Owner != command.Owner {
			return StartResult{}, ErrSessionBusy
		}
		return StartResult{SessionID: previous.SessionID, State: previous.State}, nil
	}
	if m.activeByMAC[mac] != nil {
		return StartResult{}, ErrSessionBusy
	}
	pending := m.pendingByMAC[mac]
	if pending == nil && m.store != nil {
		record, loadErr := m.store.Load(ctx, mac)
		if loadErr != nil {
			return StartResult{}, loadErr
		}
		if record != nil && record.State == "offered" &&
			record.SessionID == command.SessionID && record.StartCommandID == command.CommandID {
			pending = &pendingStart{
				MAC: record.MAC, SessionID: record.SessionID,
				CommandID: record.StartCommandID, ExpiresAt: time.Unix(record.ExpiresAt, 0),
			}
		} else if record != nil && record.State == "starting" &&
			record.SessionID == command.SessionID && record.StartCommandID == command.CommandID {
			if record.Owner == command.Owner {
				return StartResult{SessionID: command.SessionID, State: "starting"}, nil
			}
			return StartResult{}, ErrSessionBusy
		}
	}
	if pending == nil || pending.SessionID != command.SessionID || pending.CommandID != command.CommandID {
		if _, expired := m.expiredStarts[idempotencyKey]; expired {
			return StartResult{}, ErrStartTimeout
		}
		return StartResult{}, ErrSessionNotFound
	}
	if !m.now().Before(pending.ExpiresAt) {
		delete(m.pendingByMAC, mac)
		m.expiredStarts[idempotencyKey] = struct{}{}
		_, _ = m.releaseOffer(ctx, pending)
		return StartResult{}, ErrStartTimeout
	}
	session := &Session{MAC: mac, SessionID: command.SessionID, StartCommandID: command.CommandID, State: "starting", Owner: command.Owner}
	if m.store != nil {
		promoted, promoteErr := m.store.PromoteOffer(ctx, sessionRecord(session))
		if promoteErr != nil {
			return StartResult{}, promoteErr
		}
		if !promoted {
			existing, loadErr := m.store.Load(ctx, mac)
			if loadErr != nil {
				return StartResult{}, loadErr
			}
			if existing != nil && existing.State == "starting" &&
				existing.SessionID == command.SessionID && existing.StartCommandID == command.CommandID &&
				existing.Owner == command.Owner {
				return StartResult{SessionID: command.SessionID, State: "starting"}, nil
			}
			return StartResult{}, ErrSessionBusy
		}
	}
	if err := m.transport.SendDevice(ctx, mac, ControlMessage{Action: "meeting.start", SessionID: command.SessionID, CommandID: command.CommandID}); err != nil {
		_ = m.releaseSession(ctx, session)
		return StartResult{}, err
	}
	result := StartResult{SessionID: command.SessionID, State: session.State}
	delete(m.pendingByMAC, mac)
	m.activeByMAC[mac], m.starts[idempotencyKey] = session, result
	if err := m.saveCommand(ctx, "accept", session, command.CommandID, result.State); err != nil {
		return StartResult{}, err
	}
	meetingmetrics.Default.SetActiveSessions(int64(len(m.activeByMAC)))
	meetingaudit.Record(meetingaudit.Event{Action: "meeting.start", MAC: mac, UserID: command.Owner.UserID, SessionID: command.SessionID})
	return result, nil
}

func (m *MemoryManager) ExpirePending(ctx context.Context) {
	now := m.now()
	var expired []*pendingStart
	m.mu.Lock()
	for mac, pending := range m.pendingByMAC {
		if now.Before(pending.ExpiresAt) {
			continue
		}
		delete(m.pendingByMAC, mac)
		m.expiredStarts[key("accept", pending.SessionID, pending.CommandID)] = struct{}{}
		expired = append(expired, pending)
	}
	m.mu.Unlock()
	for _, pending := range expired {
		if released, err := m.releaseOffer(ctx, pending); err == nil && released {
			_ = m.transport.SendDevice(ctx, pending.MAC, ControlMessage{Action: "meeting.error", SessionID: pending.SessionID, CommandID: pending.CommandID, Code: "START_TIMEOUT"})
		}
	}
}

func (m *MemoryManager) Start(ctx context.Context, command StartCommand) (StartResult, error) {
	mac, err := validateIdentity(command.MAC, command.SessionID, command.CommandID)
	if err != nil {
		return StartResult{}, err
	}
	if command.Owner.UserID == "" || command.Owner.DeviceID == "" {
		return StartResult{}, ErrUnauthorized
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	idempotencyKey := key("start", command.SessionID, command.CommandID)
	if result, ok := m.starts[idempotencyKey]; ok {
		return result, nil
	}
	if previous, loadErr := m.loadCommand(ctx, "start", mac, command.SessionID, command.CommandID); loadErr != nil {
		return StartResult{}, loadErr
	} else if previous != nil {
		if previous.Owner != command.Owner {
			return StartResult{}, ErrSessionBusy
		}
		return StartResult{SessionID: previous.SessionID, State: previous.State}, nil
	}
	if active := m.activeByMAC[mac]; active != nil {
		return StartResult{}, ErrSessionBusy
	}
	session := &Session{MAC: mac, SessionID: command.SessionID, StartCommandID: command.CommandID, State: "starting", Owner: command.Owner}
	existing, claimed, err := m.claimSession(ctx, session)
	if err != nil {
		return StartResult{}, err
	}
	if !claimed {
		return StartResult{SessionID: existing.SessionID, State: existing.State}, nil
	}
	message := ControlMessage{Action: "meeting.start", SessionID: command.SessionID, CommandID: command.CommandID}
	if err := m.transport.SendDevice(ctx, mac, message); err != nil {
		_ = m.releaseSession(ctx, session)
		return StartResult{}, err
	}
	result := StartResult{SessionID: command.SessionID, State: session.State}
	m.activeByMAC[mac], m.starts[idempotencyKey] = session, result
	if err := m.saveCommand(ctx, "start", session, command.CommandID, result.State); err != nil {
		return StartResult{}, err
	}
	meetingmetrics.Default.SetActiveSessions(int64(len(m.activeByMAC)))
	meetingaudit.Record(meetingaudit.Event{Action: "meeting.start", MAC: mac, UserID: command.Owner.UserID, SessionID: command.SessionID})
	return result, nil
}

func (m *MemoryManager) Stop(ctx context.Context, command StopCommand) (StopResult, error) {
	mac, err := validateIdentity(command.MAC, command.SessionID, command.CommandID)
	if err != nil {
		return StopResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	idempotencyKey := key("stop", command.SessionID, command.CommandID)
	if result, ok := m.stops[idempotencyKey]; ok {
		return result, nil
	}
	if previous, loadErr := m.loadCommand(ctx, "stop", mac, command.SessionID, command.CommandID); loadErr != nil {
		return StopResult{}, loadErr
	} else if previous != nil {
		if previous.Owner != command.Owner {
			return StopResult{}, ErrUnauthorized
		}
		return StopResult{SessionID: previous.SessionID, State: previous.State}, nil
	}
	session, err := m.refreshSessionLocked(ctx, mac)
	if err != nil {
		return StopResult{}, err
	}
	if session == nil || session.SessionID != command.SessionID {
		return StopResult{}, ErrSessionNotFound
	}
	if session.Owner != command.Owner {
		return StopResult{}, ErrUnauthorized
	}
	if session.State == "stopping" {
		if session.StopCommandID == command.CommandID {
			result := StopResult{SessionID: command.SessionID, State: session.State}
			m.stops[idempotencyKey] = result
			return result, nil
		}
		return StopResult{}, ErrSessionBusy
	}
	if session.State != "starting" && session.State != "recording" {
		return StopResult{}, ErrInvalidEvent
	}
	message := ControlMessage{Action: "meeting.stop", SessionID: command.SessionID, CommandID: command.CommandID, Reason: command.Reason}
	if err := m.transport.SendDevice(ctx, mac, message); err != nil {
		return StopResult{}, err
	}
	session.State, session.StopCommandID = "stopping", command.CommandID
	if err := m.saveSession(ctx, session); err != nil {
		return StopResult{}, err
	}
	result := StopResult{SessionID: command.SessionID, State: session.State}
	m.stops[idempotencyKey] = result
	if err := m.saveCommand(ctx, "stop", session, command.CommandID, result.State); err != nil {
		return StopResult{}, err
	}
	meetingaudit.Record(meetingaudit.Event{Action: "meeting.stop", MAC: mac, UserID: command.Owner.UserID, SessionID: command.SessionID})
	return result, nil
}

func (m *MemoryManager) RequestStop(ctx context.Context, request DeviceStopRequest) error {
	mac, err := validateIdentity(request.MAC, request.SessionID, request.CommandID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.refreshSessionLocked(ctx, mac)
	if err != nil {
		return err
	}
	if session == nil || session.SessionID != request.SessionID {
		return ErrSessionNotFound
	}
	if session.State == "stopping" {
		if session.StopCommandID == request.CommandID {
			return nil
		}
		return ErrSessionBusy
	}
	if session.State != "recording" {
		return ErrInvalidEvent
	}
	message := ControlMessage{Action: "meeting.stop", SessionID: request.SessionID, CommandID: request.CommandID, Reason: request.Reason}
	if err := m.transport.SendOwner(ctx, session.Owner, message); err != nil {
		return err
	}
	session.State, session.StopCommandID = "stopping", request.CommandID
	if err := m.saveSession(ctx, session); err != nil {
		return err
	}
	return nil
}

func (m *MemoryManager) OnDeviceEvent(ctx context.Context, mac string, event Event) error {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.refreshSessionLocked(ctx, mac)
	if err != nil {
		return err
	}
	if session == nil || session.SessionID != event.SessionID {
		return ErrSessionNotFound
	}
	switch event.Action {
	case "meeting.started":
		if event.CommandID != session.StartCommandID || event.FirstSequence == nil || *event.FirstSequence != 0 || session.State != "starting" {
			return ErrInvalidEvent
		}
		session.State, session.NextSequence = "recording", *event.FirstSequence
		session.recordingStartedAt = m.now()
		if err := m.saveSession(ctx, session); err != nil {
			return err
		}
	case "meeting.stopped":
		if event.CommandID != session.StopCommandID || session.State != "stopping" {
			return ErrInvalidEvent
		}
		if session.AudioReceived {
			if event.LastSequence == nil || *event.LastSequence != session.NextSequence-1 {
				return ErrAudioGap
			}
		} else if event.LastSequence != nil {
			return ErrAudioGap
		}
	default:
		return ErrInvalidEvent
	}
	message := ControlMessage{Action: event.Action, SessionID: event.SessionID, CommandID: event.CommandID, FirstSequence: event.FirstSequence, LastSequence: event.LastSequence, Reason: event.Reason}
	if err := m.transport.SendOwner(ctx, session.Owner, message); err != nil {
		return err
	}
	if event.Action == "meeting.stopped" {
		if err := m.releaseSession(ctx, session); err != nil {
			return err
		}
		delete(m.activeByMAC, mac)
		meetingmetrics.Default.SetActiveSessions(int64(len(m.activeByMAC)))
		meetingmetrics.Default.IncCompleted()
		meetingaudit.Record(meetingaudit.Event{Action: "meeting.complete", MAC: mac, UserID: session.Owner.UserID, SessionID: session.SessionID})
	}
	return nil
}

func (m *MemoryManager) OnAudio(ctx context.Context, mac string, payload []byte) error {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return err
	}
	frame, err := wsprotocol.DecodeMeetingAudio(payload)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.ensureSessionLocked(ctx, mac)
	if err != nil {
		return err
	}
	if session == nil || session.State != "recording" || frame.SessionID.String() != session.SessionID {
		return ErrSessionNotFound
	}
	if frame.Sequence != session.NextSequence {
		if frame.Sequence > session.NextSequence {
			meetingmetrics.Default.AddFrames(0, uint64(frame.Sequence-session.NextSequence))
		}
		return ErrAudioGap
	}
	if err := m.transport.SendOwnerAudio(ctx, session.Owner, payload); err != nil {
		if m.enableReattach && errors.Is(err, ErrOwnerOffline) {
			// Phase 3 retains a bounded replay buffer for a newly authorized attachment.
		} else {
			delete(m.activeByMAC, mac)
			_ = m.releaseSession(ctx, session)
			meetingmetrics.Default.SetActiveSessions(int64(len(m.activeByMAC)))
			meetingmetrics.Default.IncInterrupted()
			_ = m.transport.SendDevice(ctx, mac, ControlMessage{Action: "meeting.stop", SessionID: session.SessionID, CommandID: session.StartCommandID, Reason: "interrupted"})
			return err
		}
	}
	now := m.now()
	if !session.firstFrameObserved {
		session.firstFrameObserved = true
		if !session.recordingStartedAt.IsZero() {
			meetingmetrics.Default.ObserveFirstFrame(now.Sub(session.recordingStartedAt))
		}
	}
	meetingmetrics.Default.AddFrames(1, 0)
	copyPayload := append([]byte(nil), payload...)
	session.audioBuffer = append(session.audioBuffer, bufferedAudio{sequence: frame.Sequence, payload: copyPayload, receivedAt: now})
	session.audioBufferBytes += len(copyPayload)
	for len(session.audioBuffer) > 0 && (now.Sub(session.audioBuffer[0].receivedAt) > audioReplayWindow || session.audioBufferBytes > audioReplayMaxBytes) {
		session.audioBufferBytes -= len(session.audioBuffer[0].payload)
		session.audioBuffer = session.audioBuffer[1:]
	}
	session.NextSequence++
	session.AudioReceived = true
	if m.store != nil {
		if err := m.store.AppendReplay(ctx, mac, session.SessionID, RedisReplayFrame{
			Sequence: frame.Sequence, Payload: copyPayload, ReceivedAtMS: now.UnixMilli(),
		}); err != nil {
			return err
		}
	}
	if err := m.saveSession(ctx, session); err != nil {
		return err
	}
	return nil
}

func (m *MemoryManager) ReattachOwner(ctx context.Context, command ReattachCommand) error {
	if !m.enableReattach {
		return ErrReattachDisabled
	}
	mac, err := validateIdentity(command.MAC, command.SessionID, command.CommandID)
	if err != nil {
		return err
	}
	if command.Owner.UserID == "" || command.Owner.DeviceID == "" {
		return ErrUnauthorized
	}
	meetingmetrics.Default.IncReconnect()
	idempotencyKey := key("reattach", command.SessionID, command.CommandID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.reattaches[idempotencyKey]; ok {
		return nil
	}
	session, err := m.refreshSessionLocked(ctx, mac)
	if err != nil {
		return err
	}
	if session == nil || session.SessionID != command.SessionID {
		return ErrSessionNotFound
	}
	if session.Owner.MAC != command.Owner.MAC || session.Owner.UserID != command.Owner.UserID || session.Owner.DeviceID != command.Owner.DeviceID || command.Owner.Generation <= session.Owner.Generation {
		return ErrUnauthorized
	}
	var replayFrom uint32
	if command.LastReceivedSequence != nil {
		replayFrom = *command.LastReceivedSequence + 1
	}
	if m.store != nil {
		stored, loadErr := m.store.LoadReplay(ctx, mac, session.SessionID, m.now().Add(-audioReplayWindow))
		if loadErr != nil {
			return loadErr
		}
		session.audioBuffer = session.audioBuffer[:0]
		session.audioBufferBytes = 0
		for _, frame := range stored {
			payload := append([]byte(nil), frame.Payload...)
			session.audioBuffer = append(session.audioBuffer, bufferedAudio{
				sequence: frame.Sequence, payload: payload, receivedAt: time.UnixMilli(frame.ReceivedAtMS),
			})
			session.audioBufferBytes += len(payload)
		}
	}
	if replayFrom == session.NextSequence {
		// The owner already consumed every frame, including the uint32 rollover
		// case where FFFFFFFF + 1 becomes zero.
		previousOwner := session.Owner
		session.Owner = command.Owner
		if err := m.saveSession(ctx, session); err != nil {
			session.Owner = previousOwner
			return err
		}
		delete(m.pendingOwners, previousOwner)
		m.reattaches[idempotencyKey] = struct{}{}
		return nil
	}
	replayIndex := -1
	for index, buffered := range session.audioBuffer {
		if buffered.sequence == replayFrom {
			replayIndex = index
			break
		}
	}
	if replayIndex < 0 {
		return ErrAudioGap
	}
	for _, buffered := range session.audioBuffer[replayIndex:] {
		if err := m.transport.SendOwnerAudio(ctx, session.Owner, buffered.payload); err != nil {
			return err
		}
	}
	previousOwner := session.Owner
	session.Owner = command.Owner
	if err := m.saveSession(ctx, session); err != nil {
		session.Owner = previousOwner
		return err
	}
	delete(m.pendingOwners, previousOwner)
	m.reattaches[idempotencyKey] = struct{}{}
	return nil
}

// OnOwnerDisconnect gives the same authenticated app identity a short,
// bounded window to reconnect with a strictly newer generation.  A later
// attachment cancels the pending cleanup by changing Session.Owner in Redis.
func (m *MemoryManager) OnOwnerDisconnect(ctx context.Context, owner Owner) {
	m.mu.Lock()
	session, err := m.refreshSessionLocked(ctx, owner.MAC)
	if err != nil || session == nil {
		m.mu.Unlock()
		return
	}
	if session.Owner != owner {
		m.mu.Unlock()
		return
	}
	if m.enableReattach {
		if _, pending := m.pendingOwners[owner]; !pending {
			m.pendingOwners[owner] = struct{}{}
			time.AfterFunc(ownerReattachWindow, func() { m.expireOwnerDisconnect(context.Background(), owner) })
		}
		m.mu.Unlock()
		return
	}
	delete(m.activeByMAC, owner.MAC)
	m.mu.Unlock()
	m.interruptOwner(ctx, session, owner)
}

func (m *MemoryManager) expireOwnerDisconnect(ctx context.Context, owner Owner) {
	m.mu.Lock()
	if _, pending := m.pendingOwners[owner]; !pending {
		m.mu.Unlock()
		return
	}
	delete(m.pendingOwners, owner)
	session, err := m.refreshSessionLocked(ctx, owner.MAC)
	if err != nil || session == nil || session.Owner != owner {
		m.mu.Unlock()
		return
	}
	delete(m.activeByMAC, owner.MAC)
	m.mu.Unlock()
	m.interruptOwner(ctx, session, owner)
}

func (m *MemoryManager) interruptOwner(ctx context.Context, session *Session, owner Owner) {
	_ = m.releaseSession(ctx, session)
	meetingmetrics.Default.SetActiveSessions(int64(m.activeCount()))
	meetingmetrics.Default.IncInterrupted()
	meetingaudit.Record(meetingaudit.Event{Action: "meeting.interrupt", MAC: owner.MAC, UserID: owner.UserID, SessionID: session.SessionID, Code: "OWNER_DISCONNECTED"})
	commandID := session.StopCommandID
	if commandID == "" {
		commandID = session.StartCommandID
	}
	_ = m.transport.SendDevice(ctx, owner.MAC, ControlMessage{Action: "meeting.stop", SessionID: session.SessionID, CommandID: commandID, Reason: "interrupted"})
}

func (m *MemoryManager) OnDeviceError(ctx context.Context, mac string, event Event) error {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return err
	}
	if event.Action != "meeting.error" {
		return ErrInvalidEvent
	}
	m.mu.Lock()
	session, err := m.refreshSessionLocked(ctx, mac)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if session == nil || session.SessionID != event.SessionID {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	if event.CommandID != session.StartCommandID && event.CommandID != session.StopCommandID {
		m.mu.Unlock()
		return ErrInvalidEvent
	}
	delete(m.activeByMAC, mac)
	m.mu.Unlock()
	_ = m.releaseSession(ctx, session)
	meetingmetrics.Default.SetActiveSessions(int64(m.activeCount()))
	meetingmetrics.Default.IncInterrupted()
	meetingaudit.Record(meetingaudit.Event{Action: "meeting.interrupt", MAC: mac, UserID: session.Owner.UserID, SessionID: session.SessionID, Code: event.Code})
	return m.transport.SendOwner(ctx, session.Owner, ControlMessage{Action: "meeting.error", SessionID: event.SessionID, CommandID: event.CommandID, Code: event.Code})
}

func (m *MemoryManager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.activeByMAC))
	for _, session := range m.activeByMAC {
		sessions = append(sessions, session)
	}
	m.activeByMAC = make(map[string]*Session)
	m.pendingByMAC = make(map[string]*pendingStart)
	m.mu.Unlock()
	meetingmetrics.Default.SetActiveSessions(0)
	for _, session := range sessions {
		_ = m.releaseSession(ctx, session)
		meetingmetrics.Default.IncInterrupted()
		commandID := session.StopCommandID
		if commandID == "" {
			commandID = session.StartCommandID
		}
		_ = m.transport.SendDevice(ctx, session.MAC, ControlMessage{Action: "meeting.stop", SessionID: session.SessionID, CommandID: commandID, Reason: "interrupted"})
		_ = m.transport.SendOwner(ctx, session.Owner, ControlMessage{Action: "meeting.error", SessionID: session.SessionID, CommandID: commandID, Code: "SERVER_DISCONNECTED"})
	}
}

// OnDeviceDisconnect terminates a device-side meeting immediately. Device
// reconnect continuation is deliberately unsupported in meeting-v1, so an
// old or partial session must never survive and accept audio from a later
// device connection.
func (m *MemoryManager) OnDeviceDisconnect(ctx context.Context, mac string) {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return
	}
	m.mu.Lock()
	active, _ := m.refreshSessionLocked(ctx, mac)
	pending := m.pendingByMAC[mac]
	if active != nil && active.State == "offered" {
		if pending == nil {
			pending = &pendingStart{
				MAC: active.MAC, SessionID: active.SessionID,
				CommandID: active.StartCommandID, ExpiresAt: time.Unix(active.ExpiresAt, 0),
			}
		}
		_, _ = m.releaseOffer(ctx, pending)
		active = nil
	}
	delete(m.activeByMAC, mac)
	delete(m.pendingByMAC, mac)
	m.mu.Unlock()
	meetingmetrics.Default.SetActiveSessions(int64(m.activeCount()))

	if active != nil {
		_ = m.releaseSession(ctx, active)
		meetingmetrics.Default.IncInterrupted()
		commandID := active.StopCommandID
		if commandID == "" {
			commandID = active.StartCommandID
		}
		_ = m.transport.SendOwner(ctx, active.Owner, ControlMessage{
			Action: "meeting.error", SessionID: active.SessionID,
			CommandID: commandID, Code: "DEVICE_OFFLINE",
		})
	}
	if pending != nil {
		_ = m.transport.SendBoundApps(ctx, mac, ControlMessage{
			Action: "meeting.error", SessionID: pending.SessionID,
			CommandID: pending.CommandID, MAC: mac, Code: "DEVICE_OFFLINE",
		})
	}
}

func (m *MemoryManager) activeCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.activeByMAC)
}

func (m *MemoryManager) Active(mac string) *Session {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Active is a control/status query, not the audio hot path. Refresh it so a
	// completion handled by another server node is not reported as still live.
	session, err := m.refreshSessionLocked(context.Background(), mac)
	if err != nil {
		return nil
	}
	if session == nil {
		return nil
	}
	if session.State == "offered" {
		return nil
	}
	copy := *session
	return &copy
}
