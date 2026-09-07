/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace stackchan::meeting {

constexpr uint8_t kProtocolVersion = 1;
constexpr std::size_t kAudioEnvelopeHeaderSize = 32;
constexpr std::size_t kMaxOpusPayloadSize = 4096;

enum class MeetingError {
    None = 0,
    NotRunning,
    InvalidCommand,
    ProtocolRejected,
    SessionConflict,
    TransportUnavailable,
    QueueFull,
    SendFailed,
    BackpressureTimeout,
    FinalDispositionUnknown,
    IncompleteAudio,
    AudioEncodeFailed,
    InputQuiesceTimeout,
    DrainTimeout,
    ProcessorJoinTimeout,
    TaskJoinTimeout,
};

enum class OuterFrameError {
    None = 0,
    TooShort,
    PayloadTooLarge,
    LengthMismatch,
};

struct OuterFrame {
    OuterFrameError error = OuterFrameError::None;
    uint8_t type = 0;
    std::vector<uint8_t> payload;
};

OuterFrame parseOuterFrame(const uint8_t* data, std::size_t size, std::size_t max_payload_size);

struct AudioFrame {
    bool final_frame = false;
    std::array<uint8_t, 16> session_id{};
    uint32_t sequence = 0;
    uint64_t capture_time_ms = 0;
    std::vector<uint8_t> opus;
};

enum class EnvelopeError {
    None = 0,
    TooShort,
    UnsupportedVersion,
    PayloadTooLarge,
    LengthMismatch,
};

struct DecodedAudioEnvelope {
    EnvelopeError error = EnvelopeError::None;
    AudioFrame frame;
};

std::vector<uint8_t> encodeAudioEnvelope(const AudioFrame& frame);
DecodedAudioEnvelope decodeAudioEnvelope(const uint8_t* data, std::size_t size);

/// Maps a configured HTTP(S) base URL to the device WebSocket endpoint.
/// Returns an empty string for unsupported schemes or a missing authority.
std::string buildWebSocketUrl(std::string_view server_base_url);
bool parseUuid(std::string_view value, std::array<uint8_t, 16>& output);

}  // namespace stackchan::meeting
