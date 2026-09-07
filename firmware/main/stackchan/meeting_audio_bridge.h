/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once

#include <array>
#include <cstdint>
#include <deque>
#include <memory>
#include <mutex>
#include <optional>
#include <string>

#include <audio/audio_service.h>
#include <protocols/protocol.h>

#include "meeting_callback_gate_state.h"
#include "meeting_protocol.h"
#include "meeting_transport_policy.h"

namespace stackchan::meeting {

enum class BridgePrepareResult {
    Ready = 0,
    AlreadyReady,
    TransportUnavailable,
    AudioInitializationFailed,
    ProcessorPreparationFailed,
};

enum class BridgeStartResult {
    Started = 0,
    NotPrepared,
    AlreadyRunning,
    InvalidSession,
    TaskStartFailed,
    StartedAckFailed,
    ProcessorStartFailed,
    TransportUnavailable,
};

enum class BridgeStopStatus {
    Complete = 0,
    NotRunning,
    IncompleteAudio,
    AudioEncodeFailed,
    InputQuiesceTimeout,
    DrainTimeout,
    ProcessorJoinTimeout,
    TaskJoinTimeout,
    TransportFailed,
};

struct BridgeStopResult {
    BridgeStopStatus status = BridgeStopStatus::NotRunning;
    bool completed = false;
    std::optional<uint32_t> last_sequence;
    std::optional<uint32_t> last_transport_accepted_sequence;
    MeetingError error = MeetingError::None;
    std::optional<uint32_t> last_attempted_sequence;
    std::optional<uint32_t> last_enqueued_sequence;
    bool terminal_outcome_known = false;
    MeetingEnqueueDisposition_t terminal_control;
    MeetingEnqueueDisposition_t error_control;
};

enum class BridgeOutcomeKind {
    Started = 0,
    Stopped,
    Error,
};

struct BridgeReplayResult {
    bool found = false;
    MeetingEnqueueDisposition_t disposition;
};

struct BridgeHealthSnapshot {
    bool healthy = true;
    bool failure_latched = false;
    MeetingError error = MeetingError::None;
    std::optional<uint32_t> last_attempted_sequence;
    std::optional<uint32_t> last_enqueued_sequence;
    std::optional<uint32_t> last_transport_accepted_sequence;
};

struct BridgeAbortResult {
    MeetingError cause = MeetingError::None;
    std::optional<uint32_t> last_attempted_sequence;
    std::optional<uint32_t> last_enqueued_sequence;
    std::optional<uint32_t> last_transport_accepted_sequence;
    MeetingEnqueueDisposition_t error_control;
};

class MeetingAudioBridge {
public:
    MeetingAudioBridge();
    ~MeetingAudioBridge();

    BridgePrepareResult prepare();
    BridgeStartResult start(const std::string& session_id, const std::string& command_id);
    BridgeStopResult stopAndDrain(const std::string& command_id, uint32_t timeout_ms = 5000);
    BridgeReplayResult replayOutcome(
        BridgeOutcomeKind kind,
        const std::string& session_id,
        const std::string& command_id
    );
    MeetingEnqueueDisposition_t enqueueErrorOutcome(
        const std::string& session_id,
        const std::string& command_id,
        const std::string& json
    );
    BridgeAbortResult abort(MeetingError cause = MeetingError::None);

    bool isPrepared() const;
    bool isRunning() const;
    BridgeHealthSnapshot inspectTransportHealth();

private:
    struct AudioCallbackGate {
        explicit AudioCallbackGate(MeetingAudioBridge* owner)
            : state(owner)
        {
        }

        std::mutex mutex;
        MeetingCallbackGateState state;
    };

    mutable std::mutex mutex_;
    std::shared_ptr<AudioCallbackGate> callback_gate_;
    std::unique_ptr<AudioService> audio_service_;
    std::deque<std::unique_ptr<AudioStreamPacket>> pending_packets_;
    std::array<uint8_t, 16> session_id_bytes_{};
    std::string session_id_;
    uint32_t next_sequence_ = 0;
    bool running_ = false;
    std::optional<uint32_t> last_attempted_sequence_;
    std::optional<uint32_t> last_enqueued_sequence_;

    struct CachedControlOutcome {
        BridgeOutcomeKind kind = BridgeOutcomeKind::Started;
        std::string session_id;
        std::string command_id;
        std::string json;
        MeetingEnqueueDisposition_t disposition;
    };
    std::deque<CachedControlOutcome> control_outcomes_;

    void onAudioPacketsAvailable();
    MeetingEnqueueDisposition_t enqueuePacketLocked(
        std::unique_ptr<AudioStreamPacket>& packet,
        bool final_frame
    );
    MeetingEnqueueDisposition_t enqueueControl(
        const char* action,
        const std::string& command_id,
        std::optional<uint32_t> sequence,
        const char* reason = nullptr
    );
    void rememberOutcome(
        BridgeOutcomeKind kind,
        const std::string& session_id,
        const std::string& command_id,
        const std::string& json,
        const MeetingEnqueueDisposition_t& disposition
    );
    bool waitForTransport(uint32_t sequence, uint32_t timeout_ms);
    bool waitForControlFlush(uint64_t ticket, uint32_t timeout_ms);
};

}  // namespace stackchan::meeting
