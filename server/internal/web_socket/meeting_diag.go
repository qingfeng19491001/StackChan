package web_socket

import (
	"context"
	"sync"
	"time"

	wsprotocol "stackChan/internal/web_socket/protocol"
)

const meetingAudioDiagInterval = 5 * time.Second

type meetingAudioDiagState struct {
	mu          sync.Mutex
	lastLog     time.Time
	frames      uint64
	lastSeq     uint32
	lastDropLog time.Time
}

var meetingAudioDiag sync.Map // mac -> *meetingAudioDiagState

func formatOptionalUint32(value *uint32) any {
	if value == nil {
		return "none"
	}
	return *value
}

func meetingAudioDiagOf(mac string) *meetingAudioDiagState {
	if existing, ok := meetingAudioDiag.Load(mac); ok {
		return existing.(*meetingAudioDiagState)
	}
	created := &meetingAudioDiagState{}
	actual, _ := meetingAudioDiag.LoadOrStore(mac, created)
	return actual.(*meetingAudioDiagState)
}

func observeMeetingAudioOK(ctx context.Context, mac string, frame wsprotocol.MeetingAudioEnvelope) {
	state := meetingAudioDiagOf(mac)
	state.mu.Lock()
	state.frames++
	state.lastSeq = frame.Sequence
	shouldLog := state.frames == 1 || time.Since(state.lastLog) >= meetingAudioDiagInterval
	frames := state.frames
	if shouldLog {
		state.lastLog = time.Now()
	}
	state.mu.Unlock()
	if !shouldLog {
		return
	}
	logger.Infof(ctx, "[SCMEET-DIAG] audio.forward mac=%s seq=%d frames=%d bytes=%d", mac, frame.Sequence, frames, len(frame.Opus))
}

func observeMeetingAudioReject(ctx context.Context, mac string, payload []byte, err error) {
	seq := any("none")
	sessionID := ""
	if frame, decodeErr := wsprotocol.DecodeMeetingAudio(payload); decodeErr == nil {
		seq = frame.Sequence
		sessionID = frame.SessionID.String()
	}
	next := any("none")
	if session := meetingManager.Active(mac); session != nil {
		next = session.NextSequence
		if sessionID == "" {
			sessionID = session.SessionID
		}
	}
	logger.Warningf(ctx, "[SCMEET-DIAG] audio.reject mac=%s session=%s seq=%v next=%v code=%s", mac, sessionID, seq, next, meetingErrorCode(err))
}

func observeMeetingAudioDrop(ctx context.Context, mac string, reason string) {
	state := meetingAudioDiagOf(mac)
	state.mu.Lock()
	shouldLog := state.lastDropLog.IsZero() || time.Since(state.lastDropLog) >= meetingAudioDiagInterval
	if shouldLog {
		state.lastDropLog = time.Now()
	}
	state.mu.Unlock()
	if !shouldLog {
		return
	}
	logger.Warningf(ctx, "[SCMEET-DIAG] audio.drop mac=%s reason=%s", mac, reason)
}

func observeMeetingDisconnect(ctx context.Context, mac string, reason string) {
	hadActive := meetingManager.Active(mac) != nil
	meetingAudioDiag.Delete(mac)
	logger.Warningf(ctx, "[SCMEET-DIAG] device.disconnect mac=%s reason=%s hadActiveSession=%v", mac, reason, hadActive)
}

func logMeetingControl(ctx context.Context, peer string, message wsprotocol.MeetingControl) {
	logger.Infof(
		ctx,
		"[SCMEET-DIAG] %s.control action=%s session=%s code=%s firstSequence=%v lastSequence=%v",
		peer,
		message.Action,
		message.SessionID,
		message.Code,
		formatOptionalUint32(message.FirstSequence),
		formatOptionalUint32(message.LastSequence),
	)
}
