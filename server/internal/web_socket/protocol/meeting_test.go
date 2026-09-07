package protocol

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/google/uuid"
)

const goldenMeetingPayloadHex = "010100112233445566778899aabbccddeeff0000002a0000018bcfe568000004deadbeef"

func TestMeetingAudioEnvelopeGoldenFixture(t *testing.T) {
	sessionID := uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff")
	envelope := MeetingAudioEnvelope{Version: MeetingProtocolVersion, Flags: MeetingFlagFinalFrame, SessionID: sessionID, Sequence: 42, CaptureTimestampMS: 1700000000000, Opus: []byte{0xde, 0xad, 0xbe, 0xef}}

	encoded, err := EncodeMeetingAudio(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(encoded); got != goldenMeetingPayloadHex {
		t.Fatalf("golden payload = %s", got)
	}
	decoded, err := DecodeMeetingAudio(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SessionID != sessionID || decoded.Sequence != 42 || decoded.CaptureTimestampMS != 1700000000000 || !decoded.FinalFrame() {
		t.Fatalf("unexpected decoded envelope: %+v", decoded)
	}
}

func TestDecodeMeetingAudioRejectsInvalidLengthsAndLimits(t *testing.T) {
	valid, _ := hex.DecodeString(goldenMeetingPayloadHex)
	for _, payload := range [][]byte{valid[:31], append(append([]byte(nil), valid...), 0)} {
		if _, err := DecodeMeetingAudio(payload); !errors.Is(err, ErrMeetingAudioLength) {
			t.Fatalf("payload length %d returned %v", len(payload), err)
		}
	}
	overLimit := make([]byte, MeetingAudioHeaderBytes+MaxOpusPayloadBytes+1)
	overLimit[0] = MeetingProtocolVersion
	overLimit[30], overLimit[31] = 0x10, 0x01
	if _, err := DecodeMeetingAudio(overLimit); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("over-limit payload returned %v", err)
	}
}

func TestParseMeetingControlNegotiation(t *testing.T) {
	hello := []byte(`{"protocolVersion":1,"action":"protocol.hello","messageId":"550e8400-e29b-41d4-a716-446655440000","role":"device","capabilities":["meeting-v1"]}`)
	message, err := ParseMeetingControl(hello)
	if err != nil || message.Action != ActionProtocolHello || !message.SupportsMeetingV1() {
		t.Fatalf("hello = %+v, err=%v", message, err)
	}
	unsupported := []byte(`{"protocolVersion":2,"action":"protocol.hello","messageId":"550e8400-e29b-41d4-a716-446655440000"}`)
	if _, err := ParseMeetingControl(unsupported); !errors.Is(err, ErrProtocolUnsupported) {
		t.Fatalf("unsupported version returned %v", err)
	}
	trailing := append(append([]byte(nil), hello...), []byte(` {}`)...)
	if _, err := ParseMeetingControl(trailing); !errors.Is(err, ErrInvalidControl) {
		t.Fatalf("trailing JSON returned %v", err)
	}
}

func TestParseMeetingReattachCarriesLastReceivedSequence(t *testing.T) {
	payload := []byte(`{"protocolVersion":1,"action":"meeting.reattach","messageId":"550e8400-e29b-41d4-a716-446655440000","commandId":"11112233-4455-4677-8899-aabbccddeeff","sessionId":"00112233-4455-6677-8899-aabbccddeeff","mac":"AABBCCDDEEFF","lastReceivedSequence":42800}`)
	control, err := ParseMeetingControl(payload)
	if err != nil {
		t.Fatal(err)
	}
	if control.LastReceivedSequence == nil || *control.LastReceivedSequence != 42800 {
		t.Fatalf("lastReceivedSequence = %v", control.LastReceivedSequence)
	}
}

func TestStrictMeetingControlRejectsWrongDirectionUnknownFieldsAndMalformedActions(t *testing.T) {
	validID := "00112233-4455-6677-8899-aabbccddeeff"
	validCommand := "11112233-4455-6677-8899-aabbccddeeff"
	validSession := "22222233-4455-6677-8899-aabbccddeeff"
	tests := []struct {
		name      string
		direction ControlDirection
		payload   string
	}{
		{"wrong hello role", DirectionApp, `{"protocolVersion":1,"action":"protocol.hello","messageId":"` + validID + `","role":"device","capabilities":["meeting-v1"]}`},
		{"unknown capability", DirectionApp, `{"protocolVersion":1,"action":"protocol.hello","messageId":"` + validID + `","role":"app","capabilities":["meeting-v2"]}`},
		{"unknown action", DirectionApp, `{"protocolVersion":1,"action":"meeting.destroy","messageId":"` + validID + `"}`},
		{"unknown field", DirectionApp, `{"protocolVersion":1,"action":"meeting.stop","messageId":"` + validID + `","commandId":"` + validCommand + `","sessionId":"` + validSession + `","mac":"AABBCCDDEEFF","extra":true}`},
		{"device sends app action", DirectionDevice, `{"protocolVersion":1,"action":"meeting.start","messageId":"` + validID + `","commandId":"` + validCommand + `","sessionId":"` + validSession + `","mac":"AABBCCDDEEFF","audio":{"codec":"opus","sampleRate":16000,"channels":1,"frameDurationMs":60}}`},
		{"malformed session uuid", DirectionApp, `{"protocolVersion":1,"action":"meeting.stop","messageId":"` + validID + `","commandId":"` + validCommand + `","sessionId":"bad","mac":"AABBCCDDEEFF"}`},
		{"invalid reason", DirectionApp, `{"protocolVersion":1,"action":"meeting.stop","messageId":"` + validID + `","commandId":"` + validCommand + `","sessionId":"` + validSession + `","mac":"AABBCCDDEEFF","reason":"secret transcript"}`},
		{"wrong audio contract", DirectionApp, `{"protocolVersion":1,"action":"meeting.start","messageId":"` + validID + `","commandId":"` + validCommand + `","sessionId":"` + validSession + `","mac":"AABBCCDDEEFF","audio":{"codec":"opus","sampleRate":48000,"channels":1,"frameDurationMs":60}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseMeetingControlForDirection([]byte(test.payload), test.direction); err == nil {
				t.Fatal("malformed or wrong-direction control was accepted")
			}
		})
	}
}

func TestStrictMeetingControlAcceptsDeviceErrorAndNullableStoppedBarrier(t *testing.T) {
	deviceError := `{"protocolVersion":1,"action":"meeting.error","messageId":"00112233-4455-6677-8899-aabbccddeeff","commandId":"11112233-4455-6677-8899-aabbccddeeff","sessionId":"22222233-4455-6677-8899-aabbccddeeff","code":"WRITE_FAILED"}`
	message, err := ParseMeetingControlForDirection([]byte(deviceError), DirectionDevice)
	if err != nil || message.Code != "WRITE_FAILED" {
		t.Fatalf("device error parse = %+v, %v", message, err)
	}
	stopped := `{"protocolVersion":1,"action":"meeting.stopped","messageId":"00112233-4455-6677-8899-aabbccddeeff","commandId":"11112233-4455-6677-8899-aabbccddeeff","sessionId":"22222233-4455-6677-8899-aabbccddeeff","lastSequence":null,"reason":"user"}`
	if _, err := ParseMeetingControlForDirection([]byte(stopped), DirectionDevice); err != nil {
		t.Fatalf("zero-frame stopped rejected: %v", err)
	}
}
