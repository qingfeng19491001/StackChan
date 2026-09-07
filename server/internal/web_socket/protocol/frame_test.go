/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package protocol

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestParseBinaryMessageRejectsMalformedFrames(t *testing.T) {
	tests := []struct {
		name    string
		msg     []byte
		wantErr error
	}{
		{name: "short header", msg: []byte{OpusMessageType, 0, 0, 0}, wantErr: ErrHeaderTooShort},
		{name: "declared payload longer than frame", msg: frameWithDeclaredLength(OpusMessageType, 3, []byte{1, 2}), wantErr: ErrLengthMismatch},
		{name: "trailing bytes", msg: frameWithDeclaredLength(OpusMessageType, 1, []byte{1, 2}), wantErr: ErrLengthMismatch},
		{name: "opus exceeds limit", msg: frameWithDeclaredLength(OpusMessageType, MaxOpusFramePayloadBytes+1, make([]byte, MaxOpusFramePayloadBytes+1)), wantErr: ErrPayloadTooLarge},
		{name: "generic payload exceeds limit", msg: frameWithDeclaredLength(0x02, MaxBinaryPayloadBytes+1, nil), wantErr: ErrPayloadTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("ParseBinaryMessage panicked: %v", recovered)
				}
			}()
			if _, _, err := ParseBinaryMessage(tt.msg); !errors.Is(err, tt.wantErr) {
				t.Fatalf("got error %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseBinaryMessageAcceptsExactLengthFrame(t *testing.T) {
	payload := []byte{1, 2, 3}
	msg := frameWithDeclaredLength(OpusMessageType, len(payload), payload)

	messageType, actual, err := ParseBinaryMessage(msg)
	if err != nil {
		t.Fatalf("valid frame was rejected: %v", err)
	}
	if messageType != OpusMessageType || string(actual) != string(payload) {
		t.Fatalf("unexpected parsed frame: type=%d payload=%v", messageType, actual)
	}
}

func frameWithDeclaredLength(messageType byte, declaredLength int, payload []byte) []byte {
	msg := make([]byte, HeaderBytes+len(payload))
	msg[0] = messageType
	binary.BigEndian.PutUint32(msg[1:HeaderBytes], uint32(declaredLength))
	copy(msg[HeaderBytes:], payload)
	return msg
}
