/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "meeting_protocol.h"

#include <algorithm>
#include <limits>

namespace stackchan::meeting {
namespace {

uint16_t readBe16(const uint8_t* data)
{
    return static_cast<uint16_t>((static_cast<uint16_t>(data[0]) << 8U) |
                                 static_cast<uint16_t>(data[1]));
}

uint32_t readBe32(const uint8_t* data)
{
    return (static_cast<uint32_t>(data[0]) << 24U) |
           (static_cast<uint32_t>(data[1]) << 16U) |
           (static_cast<uint32_t>(data[2]) << 8U) |
           static_cast<uint32_t>(data[3]);
}

uint64_t readBe64(const uint8_t* data)
{
    uint64_t value = 0;
    for (std::size_t i = 0; i < 8; ++i) {
        value = (value << 8U) | static_cast<uint64_t>(data[i]);
    }
    return value;
}

void appendBe16(std::vector<uint8_t>& output, uint16_t value)
{
    output.push_back(static_cast<uint8_t>(value >> 8U));
    output.push_back(static_cast<uint8_t>(value));
}

void appendBe32(std::vector<uint8_t>& output, uint32_t value)
{
    output.push_back(static_cast<uint8_t>(value >> 24U));
    output.push_back(static_cast<uint8_t>(value >> 16U));
    output.push_back(static_cast<uint8_t>(value >> 8U));
    output.push_back(static_cast<uint8_t>(value));
}

void appendBe64(std::vector<uint8_t>& output, uint64_t value)
{
    for (int shift = 56; shift >= 0; shift -= 8) {
        output.push_back(static_cast<uint8_t>(value >> shift));
    }
}

}  // namespace

OuterFrame parseOuterFrame(const uint8_t* data, std::size_t size, std::size_t max_payload_size)
{
    OuterFrame result;
    if (data == nullptr || size < 5) {
        result.error = OuterFrameError::TooShort;
        return result;
    }

    result.type = data[0];
    const auto payload_size = static_cast<std::size_t>(readBe32(data + 1));
    if (payload_size > max_payload_size) {
        result.error = OuterFrameError::PayloadTooLarge;
        return result;
    }
    if (payload_size != size - 5) {
        result.error = OuterFrameError::LengthMismatch;
        return result;
    }

    result.payload.assign(data + 5, data + size);
    return result;
}

std::vector<uint8_t> encodeAudioEnvelope(const AudioFrame& frame)
{
    if (frame.opus.size() > kMaxOpusPayloadSize ||
        frame.opus.size() > std::numeric_limits<uint16_t>::max()) {
        return {};
    }

    std::vector<uint8_t> output;
    output.reserve(kAudioEnvelopeHeaderSize + frame.opus.size());
    output.push_back(kProtocolVersion);
    output.push_back(frame.final_frame ? 0x01 : 0x00);
    output.insert(output.end(), frame.session_id.begin(), frame.session_id.end());
    appendBe32(output, frame.sequence);
    appendBe64(output, frame.capture_time_ms);
    appendBe16(output, static_cast<uint16_t>(frame.opus.size()));
    output.insert(output.end(), frame.opus.begin(), frame.opus.end());
    return output;
}

DecodedAudioEnvelope decodeAudioEnvelope(const uint8_t* data, std::size_t size)
{
    DecodedAudioEnvelope result;
    if (data == nullptr || size < kAudioEnvelopeHeaderSize) {
        result.error = EnvelopeError::TooShort;
        return result;
    }
    if (data[0] != kProtocolVersion) {
        result.error = EnvelopeError::UnsupportedVersion;
        return result;
    }

    const auto opus_size = static_cast<std::size_t>(readBe16(data + 30));
    if (opus_size > kMaxOpusPayloadSize) {
        result.error = EnvelopeError::PayloadTooLarge;
        return result;
    }
    if (opus_size != size - kAudioEnvelopeHeaderSize) {
        result.error = EnvelopeError::LengthMismatch;
        return result;
    }

    result.frame.final_frame = (data[1] & 0x01U) != 0;
    std::copy_n(data + 2, result.frame.session_id.size(), result.frame.session_id.begin());
    result.frame.sequence = readBe32(data + 18);
    result.frame.capture_time_ms = readBe64(data + 22);
    result.frame.opus.assign(data + kAudioEnvelopeHeaderSize, data + size);
    return result;
}

std::string buildWebSocketUrl(std::string_view server_base_url)
{
    constexpr std::string_view https_prefix = "https://";
    constexpr std::string_view http_prefix = "http://";

    std::string result;
    if (server_base_url.substr(0, https_prefix.size()) == https_prefix) {
        result = "wss://";
        server_base_url.remove_prefix(https_prefix.size());
    } else if (server_base_url.substr(0, http_prefix.size()) == http_prefix) {
        result = "ws://";
        server_base_url.remove_prefix(http_prefix.size());
    } else {
        return {};
    }

    while (!server_base_url.empty() && server_base_url.back() == '/') {
        server_base_url.remove_suffix(1);
    }
    if (server_base_url.empty() || server_base_url.front() == '/') {
        return {};
    }

    result.append(server_base_url.data(), server_base_url.size());
    result += "/stackChan/ws?deviceType=StackChan";
    return result;
}

bool parseUuid(std::string_view value, std::array<uint8_t, 16>& output)
{
    if (value.size() != 36 || value[8] != '-' || value[13] != '-' ||
        value[18] != '-' || value[23] != '-') {
        return false;
    }

    auto hexValue = [](char c) -> int {
        if (c >= '0' && c <= '9') return c - '0';
        if (c >= 'a' && c <= 'f') return c - 'a' + 10;
        if (c >= 'A' && c <= 'F') return c - 'A' + 10;
        return -1;
    };

    size_t output_index = 0;
    for (size_t input_index = 0; input_index < value.size();) {
        if (value[input_index] == '-') {
            ++input_index;
            continue;
        }
        if (input_index + 1 >= value.size() || output_index >= output.size()) {
            return false;
        }
        const int high = hexValue(value[input_index]);
        const int low = hexValue(value[input_index + 1]);
        if (high < 0 || low < 0) {
            return false;
        }
        output[output_index++] = static_cast<uint8_t>((high << 4) | low);
        input_index += 2;
    }
    return output_index == output.size();
}

}  // namespace stackchan::meeting
