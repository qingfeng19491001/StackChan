package web_socket

import (
	"errors"
	"stackChan/internal/model"
	"stackChan/internal/pairing"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type connectionRateLimits struct {
	Messages, Bytes, Control, Audio int
	Window                          time.Duration
}
type connectionRateLimiter struct {
	mu                              sync.Mutex
	now                             func() time.Time
	limits                          connectionRateLimits
	start                           time.Time
	messages, bytes, control, audio int
}

func newConnectionRateLimiter(now func() time.Time, limits connectionRateLimits) *connectionRateLimiter {
	if now == nil {
		now = time.Now
	}
	if limits.Window <= 0 {
		limits.Window = time.Second
	}
	return &connectionRateLimiter{now: now, limits: limits}
}
func (l *connectionRateLimiter) Allow(kind byte, size int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if l.start.IsZero() || now.Sub(l.start) >= l.limits.Window {
		l.start, l.messages, l.bytes, l.control, l.audio = now, 0, 0, 0, 0
	}
	if size < 0 || l.limits.Messages > 0 && l.messages >= l.limits.Messages || l.limits.Bytes > 0 && l.bytes+size > l.limits.Bytes {
		return false
	}
	if kind == MeetingControl && l.limits.Control > 0 && l.control >= l.limits.Control {
		return false
	}
	if kind == Opus && l.limits.Audio > 0 && l.audio >= l.limits.Audio {
		return false
	}
	l.messages++
	l.bytes += size
	if kind == MeetingControl {
		l.control++
	}
	if kind == Opus {
		l.audio++
	}
	return true
}

var defaultConnectionRateLimits = connectionRateLimits{
	Messages: 120,
	Bytes:    1 << 20,
	Control:  30,
	Audio:    50,
	Window:   time.Second,
}

// allowInbound applies one bounded accounting policy to both App and device
// sockets. Meeting control/audio receive their own tighter quotas; other
// legacy frames still count against the connection-wide message/byte budget.
func allowInbound(limiter *connectionRateLimiter, messageType int, payload []byte) bool {
	kind := byte(0)
	if messageType == websocket.BinaryMessage && len(payload) > 0 {
		kind = payload[0]
	}
	return limiter.Allow(kind, len(payload))
}

func authorizeAppMeetingCommand(repository pairing.Repository, client *model.AppClient, generation uint64, mac string) error {
	user, device, ticketMAC, ok := client.MeetingAuthorization(generation)
	if !ok || device != client.GetDeviceId() || ticketMAC != mac {
		return errors.New("unauthorized meeting attachment")
	}
	normalized, err := pairing.NormalizeMAC(mac)
	if err != nil || normalized != ticketMAC {
		return errors.New("unauthorized meeting mac")
	}
	owner, bound := repository.OwnerOf(normalized)
	if !bound || owner != user {
		return errors.New("unauthorized meeting owner")
	}
	return nil
}

func bothMeetingV1Selected(app *model.AppClient, appGeneration uint64, device *model.StackChanClient, deviceGeneration uint64) bool {
	return app != nil && device != nil && app.SupportsMeetingV1(appGeneration) && device.SupportsMeetingV1(deviceGeneration)
}

func canObserveMeetingState(client *model.AppClient, generation uint64) bool {
	_, _, _, ok := client.MeetingAuthorization(generation)
	return ok && client.SupportsMeetingV1(generation)
}
