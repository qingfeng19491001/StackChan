package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
)

const (
	MeetingProtocolVersion    = byte(1)
	MeetingAudioHeaderBytes   = 32
	MeetingFlagFinalFrame     = byte(1)
	MeetingControlMessageType = byte(0x1b)
	ActionProtocolHello       = "protocol.hello"
	ActionProtocolSelected    = "protocol.selected"
)

var (
	ErrMeetingAudioLength  = errors.New("meeting audio envelope length mismatch")
	ErrMeetingAudioVersion = errors.New("unsupported meeting audio version")
	ErrMeetingAudioFlags   = errors.New("invalid meeting audio flags")
	ErrProtocolUnsupported = errors.New("PROTOCOL_UNSUPPORTED")
	ErrInvalidControl      = errors.New("invalid meeting control message")
)

type MeetingAudioEnvelope struct {
	Version            byte
	Flags              byte
	SessionID          uuid.UUID
	Sequence           uint32
	CaptureTimestampMS uint64
	Opus               []byte
}

func (e MeetingAudioEnvelope) FinalFrame() bool { return e.Flags&MeetingFlagFinalFrame != 0 }

func EncodeMeetingAudio(envelope MeetingAudioEnvelope) ([]byte, error) {
	if envelope.Version != MeetingProtocolVersion {
		return nil, ErrMeetingAudioVersion
	}
	if envelope.Flags & ^MeetingFlagFinalFrame != 0 {
		return nil, ErrMeetingAudioFlags
	}
	if len(envelope.Opus) == 0 {
		return nil, ErrMeetingAudioLength
	}
	if len(envelope.Opus) > MaxOpusPayloadBytes {
		return nil, ErrPayloadTooLarge
	}
	payload := make([]byte, MeetingAudioHeaderBytes+len(envelope.Opus))
	payload[0], payload[1] = envelope.Version, envelope.Flags
	copy(payload[2:18], envelope.SessionID[:])
	binary.BigEndian.PutUint32(payload[18:22], envelope.Sequence)
	binary.BigEndian.PutUint64(payload[22:30], envelope.CaptureTimestampMS)
	binary.BigEndian.PutUint16(payload[30:32], uint16(len(envelope.Opus)))
	copy(payload[32:], envelope.Opus)
	return payload, nil
}

func DecodeMeetingAudio(payload []byte) (MeetingAudioEnvelope, error) {
	if len(payload) < MeetingAudioHeaderBytes {
		return MeetingAudioEnvelope{}, ErrMeetingAudioLength
	}
	if payload[0] != MeetingProtocolVersion {
		return MeetingAudioEnvelope{}, ErrMeetingAudioVersion
	}
	if payload[1] & ^MeetingFlagFinalFrame != 0 {
		return MeetingAudioEnvelope{}, ErrMeetingAudioFlags
	}
	opusLength := int(binary.BigEndian.Uint16(payload[30:32]))
	if opusLength > MaxOpusPayloadBytes {
		return MeetingAudioEnvelope{}, ErrPayloadTooLarge
	}
	if opusLength == 0 || len(payload) != MeetingAudioHeaderBytes+opusLength {
		return MeetingAudioEnvelope{}, ErrMeetingAudioLength
	}
	var sessionID uuid.UUID
	copy(sessionID[:], payload[2:18])
	return MeetingAudioEnvelope{
		Version: payload[0], Flags: payload[1], SessionID: sessionID,
		Sequence:           binary.BigEndian.Uint32(payload[18:22]),
		CaptureTimestampMS: binary.BigEndian.Uint64(payload[22:30]),
		Opus:               append([]byte(nil), payload[32:]...),
	}, nil
}

type MeetingControl struct {
	ProtocolVersion      int      `json:"protocolVersion"`
	Action               string   `json:"action"`
	MessageID            string   `json:"messageId"`
	Role                 string   `json:"role,omitempty"`
	Capabilities         []string `json:"capabilities,omitempty"`
	CommandID            string   `json:"commandId,omitempty"`
	SessionID            string   `json:"sessionId,omitempty"`
	MAC                  string   `json:"mac,omitempty"`
	FirstSequence        *uint32  `json:"firstSequence,omitempty"`
	LastSequence         *uint32  `json:"lastSequence,omitempty"`
	LastReceivedSequence *uint32  `json:"lastReceivedSequence,omitempty"`
	Reason               string   `json:"reason,omitempty"`
	Code                 string   `json:"code,omitempty"`
	Audio                *MeetingAudioParameters `json:"audio,omitempty"`
}

type MeetingAudioParameters struct {
	Codec           string `json:"codec"`
	SampleRate      int    `json:"sampleRate"`
	Channels        int    `json:"channels"`
	FrameDurationMS int    `json:"frameDurationMs"`
	SequenceStart   *uint32 `json:"sequenceStart,omitempty"`
}

type ControlDirection uint8

const (
	DirectionApp ControlDirection = iota + 1
	DirectionDevice
)

func (m MeetingControl) SupportsMeetingV1() bool {
	for _, capability := range m.Capabilities {
		if capability == "meeting-v1" {
			return true
		}
	}
	return false
}

func ParseMeetingControl(payload []byte) (MeetingControl, error) {
	var message MeetingControl
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return MeetingControl{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return MeetingControl{}, fmt.Errorf("%w: trailing JSON", ErrInvalidControl)
	}
	if message.ProtocolVersion != int(MeetingProtocolVersion) {
		return MeetingControl{}, ErrProtocolUnsupported
	}
	if message.Action == "" {
		return MeetingControl{}, fmt.Errorf("%w: action is required", ErrInvalidControl)
	}
	if _, err := uuid.Parse(message.MessageID); err != nil {
		return MeetingControl{}, fmt.Errorf("%w: messageId must be a UUID", ErrInvalidControl)
	}
	return message, nil
}

var stableMeetingErrorCodes = map[string]struct{}{
	"UNAUTHORIZED": {}, "NOT_BOUND": {}, "DEVICE_OFFLINE": {}, "SESSION_BUSY": {},
	"START_TIMEOUT": {}, "AUDIO_GAP": {}, "SERVER_DISCONNECTED": {}, "DEVICE_ERROR": {},
	"WRITE_FAILED": {}, "PROTOCOL_UNSUPPORTED": {}, "RATE_LIMITED": {},
}

func ParseMeetingControlForDirection(payload []byte, direction ControlDirection) (MeetingControl, error) {
	message, err := ParseMeetingControl(payload)
	if err != nil {
		return MeetingControl{}, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return MeetingControl{}, fmt.Errorf("%w: %v", ErrInvalidControl, err)
	}
	allowed := map[string]struct{}{"protocolVersion": {}, "action": {}, "messageId": {}}
	requireIdentity := func() error {
		for name, value := range map[string]string{"sessionId": message.SessionID, "commandId": message.CommandID} {
			if _, parseErr := uuid.Parse(value); parseErr != nil {
				return fmt.Errorf("%w: %s must be a UUID", ErrInvalidControl, name)
			}
			allowed[name] = struct{}{}
		}
		return nil
	}
	allowMAC := func() error {
		if len(message.MAC) != 12 {
			return fmt.Errorf("%w: normalized mac is required", ErrInvalidControl)
		}
		for _, value := range message.MAC {
			if !((value >= '0' && value <= '9') || (value >= 'A' && value <= 'F')) {
				return fmt.Errorf("%w: normalized mac is required", ErrInvalidControl)
			}
		}
		allowed["mac"] = struct{}{}
		return nil
	}
	allowReason := func() error {
		allowed["reason"] = struct{}{}
		if message.Reason != "" && message.Reason != "user" && message.Reason != "error" && message.Reason != "interrupted" {
			return fmt.Errorf("%w: invalid reason", ErrInvalidControl)
		}
		return nil
	}

	switch message.Action {
	case ActionProtocolHello:
		allowed["role"], allowed["capabilities"] = struct{}{}, struct{}{}
		expectedRole := "app"
		if direction == DirectionDevice {
			expectedRole = "device"
		}
		if message.Role != expectedRole || len(message.Capabilities) != 1 || message.Capabilities[0] != "meeting-v1" {
			return MeetingControl{}, ErrProtocolUnsupported
		}
	case "meeting.start":
		if direction != DirectionApp {
			return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl)
		}
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
		if err := allowMAC(); err != nil { return MeetingControl{}, err }
		allowed["audio"] = struct{}{}
		if message.Audio == nil || message.Audio.Codec != "opus" || message.Audio.SampleRate != 16000 || message.Audio.Channels != 1 || message.Audio.FrameDurationMS != 60 || (message.Audio.SequenceStart != nil && *message.Audio.SequenceStart != 0) {
			return MeetingControl{}, fmt.Errorf("%w: invalid audio contract", ErrInvalidControl)
		}
	case "meeting.stop":
		if direction != DirectionApp { return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl) }
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
		if err := allowMAC(); err != nil { return MeetingControl{}, err }
		if err := allowReason(); err != nil { return MeetingControl{}, err }
	case "meeting.accept":
		if direction != DirectionApp { return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl) }
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
		if err := allowMAC(); err != nil { return MeetingControl{}, err }
	case "meeting.reattach":
		if direction != DirectionApp { return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl) }
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
		if err := allowMAC(); err != nil { return MeetingControl{}, err }
		allowed["lastReceivedSequence"] = struct{}{}
	case "meeting.start-requested":
		if direction != DirectionDevice { return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl) }
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
	case "meeting.stop-requested":
		if direction != DirectionDevice { return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl) }
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
		if err := allowReason(); err != nil { return MeetingControl{}, err }
	case "meeting.started":
		if direction != DirectionDevice { return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl) }
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
		allowed["firstSequence"] = struct{}{}
		if message.FirstSequence == nil || *message.FirstSequence != 0 { return MeetingControl{}, fmt.Errorf("%w: firstSequence must be zero", ErrInvalidControl) }
	case "meeting.stopped":
		if direction != DirectionDevice { return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl) }
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
		allowed["lastSequence"] = struct{}{}
		if err := allowReason(); err != nil { return MeetingControl{}, err }
	case "meeting.error":
		if direction != DirectionDevice { return MeetingControl{}, fmt.Errorf("%w: wrong action direction", ErrInvalidControl) }
		if err := requireIdentity(); err != nil { return MeetingControl{}, err }
		allowed["code"] = struct{}{}
		if _, ok := stableMeetingErrorCodes[message.Code]; !ok { return MeetingControl{}, fmt.Errorf("%w: invalid error code", ErrInvalidControl) }
	default:
		return MeetingControl{}, fmt.Errorf("%w: unknown action", ErrInvalidControl)
	}
	for field := range raw {
		if _, ok := allowed[field]; !ok {
			return MeetingControl{}, fmt.Errorf("%w: field %s is not allowed for %s", ErrInvalidControl, field, message.Action)
		}
	}
	return message, nil
}
