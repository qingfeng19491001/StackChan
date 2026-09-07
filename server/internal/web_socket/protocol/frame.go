/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	HeaderBytes              = 5
	MaxOpusPayloadBytes      = 4 * 1024
	MaxOpusFramePayloadBytes = MeetingAudioHeaderBytes + MaxOpusPayloadBytes
	MaxBinaryPayloadBytes    = 4 * 1024 * 1024
	OpusMessageType          = byte(0x01)
)

var (
	ErrHeaderTooShort  = errors.New("binary frame header is shorter than 5 bytes")
	ErrLengthMismatch  = errors.New("binary frame payload length does not match header")
	ErrPayloadTooLarge = errors.New("binary frame payload exceeds limit")
)

// ParseBinaryMessage validates the complete outer frame before returning a
// payload view. In particular, it never slices using an untrusted length.
func ParseBinaryMessage(msg []byte) (messageType byte, payload []byte, err error) {
	if len(msg) < HeaderBytes {
		return 0, nil, ErrHeaderTooShort
	}

	messageType = msg[0]
	declaredLength := binary.BigEndian.Uint32(msg[1:HeaderBytes])
	limit := uint32(MaxBinaryPayloadBytes)
	if messageType == OpusMessageType {
		limit = MaxOpusFramePayloadBytes
	}
	if declaredLength > limit {
		return 0, nil, fmt.Errorf("%w: type=%d length=%d limit=%d", ErrPayloadTooLarge, messageType, declaredLength, limit)
	}

	actualLength := len(msg) - HeaderBytes
	if uint64(actualLength) != uint64(declaredLength) {
		return 0, nil, fmt.Errorf("%w: declared=%d actual=%d", ErrLengthMismatch, declaredLength, actualLength)
	}

	return messageType, msg[HeaderBytes:], nil
}
