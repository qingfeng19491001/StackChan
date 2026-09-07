/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include <array>
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <string_view>
#include <string>
#include <vector>

#include <stackchan/meeting_protocol.h>

namespace {

using stackchan::meeting::AudioFrame;
using stackchan::meeting::EnvelopeError;
using stackchan::meeting::OuterFrameError;

[[noreturn]] void fail(std::string_view label)
{
    std::cerr << label << '\n';
    std::exit(1);
}

void expect(bool condition, std::string_view label)
{
    if (!condition) {
        fail(label);
    }
}

void testOuterFrameRequiresExactLength()
{
    const std::array<uint8_t, 8> valid = {0x1B, 0x00, 0x00, 0x00, 0x03, 'a', 'b', 'c'};
    auto parsed = stackchan::meeting::parseOuterFrame(valid.data(), valid.size(), 4096);
    expect(parsed.error == OuterFrameError::None, "valid outer frame rejected");
    expect(parsed.type == 0x1B, "outer type differs");
    expect(parsed.payload.size() == 3, "outer payload size differs");

    const std::array<uint8_t, 7> truncated = {0x1B, 0x00, 0x00, 0x00, 0x03, 'a', 'b'};
    expect(
        stackchan::meeting::parseOuterFrame(truncated.data(), truncated.size(), 4096).error ==
            OuterFrameError::LengthMismatch,
        "truncated outer frame accepted"
    );

    const std::array<uint8_t, 9> trailing = {0x1B, 0x00, 0x00, 0x00, 0x03, 'a', 'b', 'c', 'x'};
    expect(
        stackchan::meeting::parseOuterFrame(trailing.data(), trailing.size(), 4096).error ==
            OuterFrameError::LengthMismatch,
        "outer frame with trailing bytes accepted"
    );

    const std::array<uint8_t, 5> oversized = {0x01, 0x00, 0x00, 0x10, 0x01};
    expect(
        stackchan::meeting::parseOuterFrame(oversized.data(), oversized.size(), 4096).error ==
            OuterFrameError::PayloadTooLarge,
        "oversized outer frame was not rejected before payload access"
    );
}

void testMeetingAudioGoldenEnvelope()
{
    AudioFrame frame;
    frame.final_frame = true;
    frame.session_id = {
        0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
        0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF,
    };
    frame.sequence = 42;
    frame.capture_time_ms = 1700000000000ULL;
    frame.opus = {0xDE, 0xAD, 0xBE, 0xEF};

    const std::vector<uint8_t> expected = {
        0x01, 0x01, 0x00, 0x11, 0x22, 0x33, 0x44, 0x55,
        0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD,
        0xEE, 0xFF, 0x00, 0x00, 0x00, 0x2A, 0x00, 0x00,
        0x01, 0x8B, 0xCF, 0xE5, 0x68, 0x00, 0x00, 0x04,
        0xDE, 0xAD, 0xBE, 0xEF,
    };

    auto encoded = stackchan::meeting::encodeAudioEnvelope(frame);
    expect(encoded == expected, "meeting audio golden envelope differs");

    auto decoded = stackchan::meeting::decodeAudioEnvelope(encoded.data(), encoded.size());
    expect(decoded.error == EnvelopeError::None, "golden envelope rejected");
    expect(decoded.frame.final_frame, "final-frame flag lost");
    expect(decoded.frame.session_id == frame.session_id, "session UUID differs");
    expect(decoded.frame.sequence == 42, "sequence differs");
    expect(decoded.frame.capture_time_ms == 1700000000000ULL, "capture time differs");
    expect(decoded.frame.opus == frame.opus, "Opus bytes differ");
}

void testMeetingAudioEnvelopeLimitsAndLengths()
{
    std::vector<uint8_t> tooShort(31, 0);
    expect(
        stackchan::meeting::decodeAudioEnvelope(tooShort.data(), tooShort.size()).error ==
            EnvelopeError::TooShort,
        "short meeting envelope accepted"
    );

    std::vector<uint8_t> wrongVersion(32, 0);
    wrongVersion[0] = 2;
    expect(
        stackchan::meeting::decodeAudioEnvelope(wrongVersion.data(), wrongVersion.size()).error ==
            EnvelopeError::UnsupportedVersion,
        "unknown meeting envelope version accepted"
    );

    std::vector<uint8_t> mismatched(32, 0);
    mismatched[0] = 1;
    mismatched[30] = 0;
    mismatched[31] = 1;
    expect(
        stackchan::meeting::decodeAudioEnvelope(mismatched.data(), mismatched.size()).error ==
            EnvelopeError::LengthMismatch,
        "meeting envelope with missing Opus byte accepted"
    );

    AudioFrame oversized;
    oversized.opus.resize(4097, 0x7F);
    expect(
        stackchan::meeting::encodeAudioEnvelope(oversized).empty(),
        "oversized Opus payload encoded"
    );
}

void testMeetingAudioSequenceRolloverValue()
{
    AudioFrame frame;
    frame.session_id = {
        0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
        0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF,
    };
    frame.sequence = UINT32_MAX;
    frame.opus = {0x01};
    const auto encoded = stackchan::meeting::encodeAudioEnvelope(frame);
    const auto decoded = stackchan::meeting::decodeAudioEnvelope(encoded.data(), encoded.size());
    expect(decoded.error == EnvelopeError::None, "max sequence envelope rejected");
    expect(decoded.frame.sequence == UINT32_MAX, "max sequence value changed");
    uint32_t next = decoded.frame.sequence;
    ++next;
    expect(next == 0, "uint32 sequence did not roll over to zero");
}

void testWebSocketUrlMapping()
{
    using stackchan::meeting::buildWebSocketUrl;

    expect(
        buildWebSocketUrl("https://stackchan.example.com") ==
            "wss://stackchan.example.com/stackChan/ws?deviceType=StackChan",
        "https server URL was not mapped to wss"
    );
    expect(
        buildWebSocketUrl("http://10.0.30.218:12800/") ==
            "ws://10.0.30.218:12800/stackChan/ws?deviceType=StackChan",
        "http server URL was not mapped to ws"
    );
    expect(buildWebSocketUrl("ftp://invalid.example").empty(), "unsupported URL scheme accepted");
    expect(buildWebSocketUrl("https://").empty(), "URL without authority accepted");
}

void testUuidParsing()
{
    std::array<uint8_t, 16> parsed{};
    expect(
        stackchan::meeting::parseUuid("00112233-4455-6677-8899-aabbccddeeff", parsed),
        "valid UUID rejected"
    );
    const std::array<uint8_t, 16> expected = {
        0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77,
        0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF,
    };
    expect(parsed == expected, "UUID network-order bytes differ");
    expect(!stackchan::meeting::parseUuid("00112233", parsed), "short UUID accepted");
    expect(
        !stackchan::meeting::parseUuid("00112233-4455-6677-8899-aabbccddeezz", parsed),
        "non-hex UUID accepted"
    );
}

}  // namespace

int main()
{
    testOuterFrameRequiresExactLength();
    testMeetingAudioGoldenEnvelope();
    testMeetingAudioEnvelopeLimitsAndLengths();
    testMeetingAudioSequenceRolloverValue();
    testWebSocketUrlMapping();
    testUuidParsing();
    return 0;
}
