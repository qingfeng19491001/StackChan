/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "meeting_audio_bridge.h"

#include <ArduinoJson.hpp>
#include <board.h>
#include <cstdio>
#include <esp_log.h>
#include <esp_system.h>
#include <esp_timer.h>
#include <hal/hal.h>

#include "meeting_protocol.h"
#include "meeting_stop_policy.h"
#include "meeting_outcome_replay_policy.h"

namespace stackchan::meeting {
namespace {

constexpr const char* TAG = "MeetingAudioBridge";
constexpr size_t kMaxBridgePendingFrames = 2;
constexpr size_t kMaxCachedControlOutcomes = 16;
constexpr int64_t kDiagHealthIntervalUs = 5'000'000;
constexpr int64_t kDiagEnqueueFailIntervalUs = 2'000'000;

const char* FormatOptionalU32(const std::optional<uint32_t>& value, char (&buf)[16])
{
    if (!value.has_value()) {
        return "none";
    }
    std::snprintf(buf, sizeof(buf), "%u", *value);
    return buf;
}

std::string createMessageId()
{
    char message_id[37] = {};
    std::snprintf(
        message_id,
        sizeof(message_id),
        "%08lx-%04lx-4%03lx-%04lx-%08lx%04lx",
        esp_random(),
        esp_random() & 0xFFFFU,
        esp_random() & 0x0FFFU,
        (esp_random() & 0x3FFFU) | 0x8000U,
        esp_random(),
        esp_random() & 0xFFFFU
    );
    return message_id;
}

}  // namespace

MeetingAudioBridge::MeetingAudioBridge()
    : callback_gate_(std::make_shared<AudioCallbackGate>(this))
{
}

MeetingAudioBridge::~MeetingAudioBridge()
{
    {
        std::lock_guard<std::mutex> callback_lock(callback_gate_->mutex);
        callback_gate_->state.Disable();
    }
    abort();
    std::lock_guard<std::mutex> lock(mutex_);
    if (audio_service_ != nullptr && !audio_service_->CanDestroy()) {
        ESP_LOGE(TAG, "Leaking a live AudioService after cooperative join timeout to prevent use-after-free");
        audio_service_.release();
    }
}

BridgePrepareResult MeetingAudioBridge::prepare()
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (audio_service_ != nullptr) {
        return BridgePrepareResult::AlreadyReady;
    }

    auto audio_service = std::make_unique<AudioService>(true);
    if (audio_service->Initialize(Board::GetInstance().GetAudioCodec()) != AudioServiceInitResult::Ok) {
        return BridgePrepareResult::AudioInitializationFailed;
    }

    const auto processor_prepare = audio_service->PrepareVoiceProcessing();
    if (processor_prepare != AudioServicePrepareResult::Prepared &&
        processor_prepare != AudioServicePrepareResult::AlreadyPrepared) {
        return BridgePrepareResult::ProcessorPreparationFailed;
    }

    AudioServiceCallbacks callbacks;
    auto callback_gate = callback_gate_;
    callbacks.on_send_queue_available = [callback_gate]() {
        std::lock_guard<std::mutex> callback_lock(callback_gate->mutex);
        auto* owner = static_cast<MeetingAudioBridge*>(callback_gate->state.Owner());
        if (owner != nullptr) {
            owner->onAudioPacketsAvailable();
        }
    };
    audio_service->SetCallbacks(callbacks);
    audio_service_ = std::move(audio_service);
    pending_packets_.clear();
    next_sequence_ = 0;
    last_attempted_sequence_.reset();
    last_enqueued_sequence_.reset();
    return BridgePrepareResult::Ready;
}

BridgeStartResult MeetingAudioBridge::start(
    const std::string& session_id,
    const std::string& command_id
) {
    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (audio_service_ == nullptr) {
            return BridgeStartResult::NotPrepared;
        }
        if (running_) {
            return BridgeStartResult::AlreadyRunning;
        }
        if (!parseUuid(session_id, session_id_bytes_)) {
            return BridgeStartResult::InvalidSession;
        }
        session_id_ = session_id;
        next_sequence_ = 0;
        pending_packets_.clear();
        last_attempted_sequence_.reset();
        last_enqueued_sequence_.reset();
        last_diag_log_us_ = 0;
        last_diag_enqueue_fail_us_ = 0;
        last_diag_enqueued_.reset();
        if (!GetHAL().beginMeetingTransportSession()) {
            ESP_LOGW(TAG, "[SCMEET-DIAG] start.rejected session=%s reason=TransportUnavailable", session_id.c_str());
            return BridgeStartResult::TransportUnavailable;
        }
        const auto start_result = audio_service_->Start();
        if (start_result != AudioServiceStartResult::Started &&
            start_result != AudioServiceStartResult::AlreadyStarted) {
            ESP_LOGW(TAG, "[SCMEET-DIAG] start.rejected session=%s reason=TaskStartFailed", session_id.c_str());
            return BridgeStartResult::TaskStartFailed;
        }
        running_ = true;
    }

    const auto started_control = enqueueControl("meeting.started", command_id, 0);
    if (!started_control.enqueued && !started_control.locally_accepted) {
        ESP_LOGW(
            TAG,
            "[SCMEET-DIAG] start.ack_failed session=%s enqueued=%d accepted=%d",
            session_id.c_str(),
            started_control.enqueued ? 1 : 0,
            started_control.locally_accepted ? 1 : 0
        );
        abort();
        return BridgeStartResult::StartedAckFailed;
    }

    std::lock_guard<std::mutex> lock(mutex_);
    const auto processor_result = audio_service_->EnableVoiceProcessing(true);
    if (processor_result != AudioServiceVoiceResult::Started &&
        processor_result != AudioServiceVoiceResult::AlreadyInState) {
        running_ = false;
        audio_service_->Stop();
        ESP_LOGW(TAG, "[SCMEET-DIAG] start.rejected session=%s reason=ProcessorStartFailed", session_id.c_str());
        return BridgeStartResult::ProcessorStartFailed;
    }
    ESP_LOGI(TAG, "[SCMEET-DIAG] start.ok session=%s", session_id.c_str());
    return BridgeStartResult::Started;
}

BridgeStopResult MeetingAudioBridge::stopAndDrain(
    const std::string& command_id,
    uint32_t timeout_ms
) {
    AudioService* audio_service = nullptr;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        if (!running_ || audio_service_ == nullptr) {
            return {
                BridgeStopStatus::NotRunning,
                false,
                std::nullopt,
                std::nullopt,
                MeetingError::NotRunning,
            };
        }
        audio_service = audio_service_.get();
    }

    const int64_t deadline_us = esp_timer_get_time() + static_cast<int64_t>(timeout_ms) * 1000;
    const auto drain_result = audio_service->BeginDrain(timeout_ms);
    std::optional<uint32_t> last_sequence;
    bool transport_ok = true;
    const bool producers_quiescent = drain_result == AudioServiceDrainResult::Complete ||
        drain_result == AudioServiceDrainResult::IncompleteProcessorTail ||
        drain_result == AudioServiceDrainResult::EncoderFailed;
    if (producers_quiescent) {
        onAudioPacketsAvailable();
        while (true) {
            size_t pending_count = 0;
            {
                std::lock_guard<std::mutex> lock(mutex_);
                pending_count = pending_packets_.size();
            }
            if (pending_count <= 1) break;
            const auto transport = GetHAL().getMeetingTransportSnapshot();
            if (transport.failureLatched || esp_timer_get_time() >= deadline_us) {
                transport_ok = false;
                break;
            }
            GetHAL().serviceMeetingTransport();
            vTaskDelay(pdMS_TO_TICKS(10));
            onAudioPacketsAvailable();
        }
        while (transport_ok) {
            bool has_pending = false;
            MeetingEnqueueDisposition_t final_enqueue;
            {
                std::lock_guard<std::mutex> lock(mutex_);
                has_pending = !pending_packets_.empty();
                if (has_pending) {
                    final_enqueue = enqueuePacketLocked(pending_packets_.front(), true);
                    if (final_enqueue.enqueued || final_enqueue.locally_accepted) {
                        pending_packets_.pop_front();
                        last_sequence = last_enqueued_sequence_;
                        has_pending = false;
                    }
                } else {
                    // next_sequence_ can legitimately wrap to zero. The last
                    // successfully accepted frame is the authoritative stop
                    // barrier; zero must not be mistaken for "no audio".
                    last_sequence = last_enqueued_sequence_;
                }
            }
            if (!has_pending) break;
            const auto transport = GetHAL().getMeetingTransportSnapshot();
            if (transport.failureLatched || esp_timer_get_time() >= deadline_us) {
                transport_ok = false;
                break;
            }
            GetHAL().serviceMeetingTransport();
            vTaskDelay(pdMS_TO_TICKS(10));
        }
        const int64_t remaining_us = deadline_us - esp_timer_get_time();
        if (transport_ok && last_sequence.has_value()) {
            transport_ok = waitForTransport(
                *last_sequence,
                remaining_us > 0 ? static_cast<uint32_t>(remaining_us / 1000) : 0
            );
        }
    }
    {
        std::lock_guard<std::mutex> lock(mutex_);
        running_ = false;
    }

    const int64_t remaining_us = deadline_us - esp_timer_get_time();
    const auto audio_stop = audio_service->FinishStop(
        remaining_us > 0 ? static_cast<uint32_t>(remaining_us / 1000) : 0
    );

    auto acceptedSequence = []() -> std::optional<uint32_t> {
        const auto transport = GetHAL().getMeetingTransportSnapshot();
        return transport.hasLocalSocketAcceptedSequence
            ? std::optional<uint32_t>(transport.lastLocalSocketAcceptedSequence)
            : std::nullopt;
    };
    std::optional<uint32_t> attempted_evidence;
    std::optional<uint32_t> enqueued_evidence;
    MeetingEnqueueDisposition_t terminal_control;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        attempted_evidence = last_attempted_sequence_;
        enqueued_evidence = last_enqueued_sequence_;
    }
    auto makeResult = [&](BridgeStopStatus status, MeetingError error, bool terminal_known = false) {
        char last_buf[16];
        char accepted_buf[16];
        const auto accepted = acceptedSequence();
        ESP_LOGW(
            TAG,
            "[SCMEET-DIAG] stop.incomplete status=%d error=%d lastSequence=%s accepted=%s terminalKnown=%d transportOk=%d",
            static_cast<int>(status),
            static_cast<int>(error),
            FormatOptionalU32(last_sequence, last_buf),
            FormatOptionalU32(accepted, accepted_buf),
            terminal_known ? 1 : 0,
            transport_ok ? 1 : 0
        );
        return BridgeStopResult{
            status,
            false,
            last_sequence,
            accepted,
            error,
            attempted_evidence,
            enqueued_evidence,
            terminal_known,
            terminal_control,
        };
    };

    if (drain_result == AudioServiceDrainResult::InputQuiesceTimeout) {
        return makeResult(BridgeStopStatus::InputQuiesceTimeout, MeetingError::InputQuiesceTimeout);
    }
    if (drain_result == AudioServiceDrainResult::EncoderDrainTimeout) {
        return makeResult(BridgeStopStatus::DrainTimeout, MeetingError::DrainTimeout);
    }
    if (audio_stop == AudioServiceStopResult::ProcessorJoinTimeout) {
        return makeResult(BridgeStopStatus::ProcessorJoinTimeout, MeetingError::ProcessorJoinTimeout);
    }
    if (audio_stop == AudioServiceStopResult::TaskJoinTimeout) {
        return makeResult(BridgeStopStatus::TaskJoinTimeout, MeetingError::TaskJoinTimeout);
    }
    if (!transport_ok) {
        return makeResult(BridgeStopStatus::TransportFailed, MeetingError::FinalDispositionUnknown);
    }

    auto sendTerminalControl = [&](const char* reason) {
        terminal_control = enqueueControl("meeting.stopped", command_id, last_sequence, reason);
        if (terminal_control.locally_accepted) return true;
        if (!terminal_control.enqueued || terminal_control.ticket == 0) return false;
        const int64_t terminal_remaining_us = deadline_us - esp_timer_get_time();
        const bool flushed = waitForControlFlush(
            terminal_control.ticket,
            terminal_remaining_us > 0 ? static_cast<uint32_t>(terminal_remaining_us / 1000) : 0
        );
        if (ResolveIncompleteTerminal(terminal_control.enqueued, flushed) ==
            MeetingIncompleteTerminalDisposition::IncompleteAudioReported) {
            terminal_control.disposition = MeetingMessageDisposition::LocalSocketAccepted;
            terminal_control.locally_accepted = true;
            terminal_control.enqueued = false;
            terminal_control.error = MeetingTransportError::None;
            return true;
        }
        return false;
    };

    if (drain_result == AudioServiceDrainResult::EncoderFailed) {
        if (!sendTerminalControl("error")) {
            return makeResult(BridgeStopStatus::TransportFailed, MeetingError::FinalDispositionUnknown);
        }
        return makeResult(BridgeStopStatus::AudioEncodeFailed, MeetingError::AudioEncodeFailed, true);
    }
    if (drain_result == AudioServiceDrainResult::IncompleteProcessorTail) {
        if (!sendTerminalControl("error")) {
            return makeResult(BridgeStopStatus::TransportFailed, MeetingError::FinalDispositionUnknown);
        }
        return makeResult(BridgeStopStatus::IncompleteAudio, MeetingError::IncompleteAudio, true);
    }
    if (!sendTerminalControl("user")) {
        return makeResult(BridgeStopStatus::TransportFailed, MeetingError::FinalDispositionUnknown);
    }
    const auto accepted_sequence = acceptedSequence();
    char last_buf[16];
    char accepted_buf[16];
    if (last_sequence.has_value() && accepted_sequence != last_sequence) {
        ESP_LOGW(
            TAG,
            "[SCMEET-DIAG] stop.sequence_mismatch lastSequence=%s accepted=%s",
            FormatOptionalU32(last_sequence, last_buf),
            FormatOptionalU32(accepted_sequence, accepted_buf)
        );
        return BridgeStopResult{
            BridgeStopStatus::TransportFailed,
            false,
            last_sequence,
            accepted_sequence,
            MeetingError::FinalDispositionUnknown,
            attempted_evidence,
            enqueued_evidence,
            true,
            terminal_control,
        };
    }
    ESP_LOGI(
        TAG,
        "[SCMEET-DIAG] stop.ok lastSequence=%s accepted=%s",
        FormatOptionalU32(last_sequence, last_buf),
        FormatOptionalU32(accepted_sequence, accepted_buf)
    );
    return {
        BridgeStopStatus::Complete,
        true,
        last_sequence,
        accepted_sequence,
        MeetingError::None,
        attempted_evidence,
        enqueued_evidence,
        true,
        terminal_control,
    };
}

BridgeReplayResult MeetingAudioBridge::replayOutcome(
    BridgeOutcomeKind kind,
    const std::string& session_id,
    const std::string& command_id
) {
    std::string json;
    MeetingEnqueueDisposition_t previous;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        for (auto it = control_outcomes_.rbegin(); it != control_outcomes_.rend(); ++it) {
            if (it->kind == kind && it->session_id == session_id &&
                it->command_id == command_id) {
                json = it->json;
                previous = it->disposition;
                break;
            }
        }
    }
    const auto transport = GetHAL().getMeetingTransportSnapshot();
    const auto action = ResolveMeetingOutcomeReplay(
        !json.empty(),
        previous.ticket,
        previous.enqueued,
        transport.lastLocalSocketAcceptedTicket,
        transport.failureLatched
    );
    if (action == MeetingOutcomeReplayAction::Missing) return {};
    if (action == MeetingOutcomeReplayAction::TerminalFailure) {
        previous.disposition = MeetingMessageDisposition::TerminalFailure;
        previous.error = transport.error;
        previous.enqueued = false;
        previous.locally_accepted = false;
        return {true, previous};
    }
    if (action == MeetingOutcomeReplayAction::KeepPending) {
        return {true, previous};
    }

    const auto replayed = GetHAL().enqueueMeetingControl(json);
    rememberOutcome(kind, session_id, command_id, json, replayed);
    return {true, replayed};
}

MeetingEnqueueDisposition_t MeetingAudioBridge::enqueueErrorOutcome(
    const std::string& session_id,
    const std::string& command_id,
    const std::string& json
) {
    const auto disposition = GetHAL().enqueueMeetingControl(json);
    rememberOutcome(BridgeOutcomeKind::Error, session_id, command_id, json, disposition);
    return disposition;
}

BridgeAbortResult MeetingAudioBridge::abort(MeetingError cause)
{
    BridgeAbortResult result;
    result.cause = cause;
    const auto transport = GetHAL().getMeetingTransportSnapshot();
    if (transport.hasLocalSocketAcceptedSequence) {
        result.last_transport_accepted_sequence = transport.lastLocalSocketAcceptedSequence;
    }
    AudioService* audio_service = nullptr;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        result.last_attempted_sequence = last_attempted_sequence_;
        result.last_enqueued_sequence = last_enqueued_sequence_;
        running_ = false;
        pending_packets_.clear();
        audio_service = audio_service_.get();
    }
    if (audio_service != nullptr) {
        audio_service->Stop();
        std::lock_guard<std::mutex> lock(mutex_);
        if (audio_service_.get() == audio_service && audio_service->CanDestroy()) {
            audio_service_.reset();
        }
    }
    char attempted_buf[16];
    char enqueued_buf[16];
    char accepted_buf[16];
    ESP_LOGW(
        TAG,
        "[SCMEET-DIAG] abort cause=%d attempted=%s enqueued=%s accepted=%s",
        static_cast<int>(cause),
        FormatOptionalU32(result.last_attempted_sequence, attempted_buf),
        FormatOptionalU32(result.last_enqueued_sequence, enqueued_buf),
        FormatOptionalU32(result.last_transport_accepted_sequence, accepted_buf)
    );
    return result;
}

bool MeetingAudioBridge::isPrepared() const
{
    std::lock_guard<std::mutex> lock(mutex_);
    return audio_service_ != nullptr;
}

bool MeetingAudioBridge::isRunning() const
{
    std::lock_guard<std::mutex> lock(mutex_);
    return running_;
}

BridgeHealthSnapshot MeetingAudioBridge::inspectTransportHealth()
{
    onAudioPacketsAvailable();
    GetHAL().serviceMeetingTransport();
    const auto transport = GetHAL().getMeetingTransportSnapshot();
    BridgeHealthSnapshot health;
    health.failure_latched = transport.failureLatched ||
        transport.error == MeetingTransportError::ServiceUnavailable;
    health.healthy = !health.failure_latched;
    if (health.failure_latched) {
        if (transport.error == MeetingTransportError::PressureTimeout) {
            health.error = MeetingError::BackpressureTimeout;
        } else if (transport.error == MeetingTransportError::QueueFull) {
            health.error = MeetingError::QueueFull;
        } else if (transport.error == MeetingTransportError::SendFailed) {
            health.error = MeetingError::SendFailed;
        } else {
            health.error = MeetingError::TransportUnavailable;
        }
    }
    if (transport.hasLocalSocketAcceptedSequence) {
        health.last_transport_accepted_sequence = transport.lastLocalSocketAcceptedSequence;
    }
    char attempted_buf[16];
    char enqueued_buf[16];
    char accepted_buf[16];
    size_t pending = 0;
    uint32_t next_sequence = 0;
    bool should_log = false;
    bool stalled = false;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        health.last_attempted_sequence = last_attempted_sequence_;
        health.last_enqueued_sequence = last_enqueued_sequence_;
        pending = pending_packets_.size();
        next_sequence = next_sequence_;
        const int64_t now_us = esp_timer_get_time();
        should_log = running_ && (last_diag_log_us_ == 0 || now_us - last_diag_log_us_ >= kDiagHealthIntervalUs);
        if (should_log) {
            stalled = last_diag_log_us_ != 0 && last_diag_enqueued_ == last_enqueued_sequence_;
            last_diag_log_us_ = now_us;
            last_diag_enqueued_ = last_enqueued_sequence_;
        }
    }
    if (should_log) {
        ESP_LOGI(
            TAG,
            "[SCMEET-DIAG] audio.health pending=%u next=%u attempted=%s enqueued=%s accepted=%s latched=%d stalled=%d",
            static_cast<unsigned>(pending),
            next_sequence,
            FormatOptionalU32(health.last_attempted_sequence, attempted_buf),
            FormatOptionalU32(health.last_enqueued_sequence, enqueued_buf),
            FormatOptionalU32(health.last_transport_accepted_sequence, accepted_buf),
            health.failure_latched ? 1 : 0,
            stalled ? 1 : 0
        );
    }
    return health;
}

void MeetingAudioBridge::onAudioPacketsAvailable()
{
    std::lock_guard<std::mutex> lock(mutex_);
    if (!running_ || audio_service_ == nullptr) {
        return;
    }

    while (true) {
        while (pending_packets_.size() < kMaxBridgePendingFrames) {
            auto packet = audio_service_->PopPacketFromSendQueue();
            if (packet == nullptr) break;
            pending_packets_.push_back(std::move(packet));
        }
        // Retain one packet so stopAndDrain can mark the last real packet final.
        // The two-frame cap prevents this bridge from becoming an unbounded
        // secondary buffer while the HAL queue applies backpressure.
        if (pending_packets_.size() <= 1) return;
        const auto disposition = enqueuePacketLocked(pending_packets_.front(), false);
        if (!disposition.enqueued && !disposition.locally_accepted) {
            return;
        }
        pending_packets_.pop_front();
    }
}

MeetingEnqueueDisposition_t MeetingAudioBridge::enqueuePacketLocked(
    std::unique_ptr<AudioStreamPacket>& packet,
    bool final_frame
) {
    if (packet == nullptr) {
        return {
            MeetingMessageDisposition::Rejected,
            MeetingTransportError::InvalidPayload,
            0,
            false,
            false,
        };
    }
    AudioFrame frame;
    frame.final_frame = final_frame;
    frame.session_id = session_id_bytes_;
    frame.sequence = next_sequence_;
    frame.capture_time_ms = packet->capture_timestamp_ms;
    frame.opus = packet->payload;
    last_attempted_sequence_ = next_sequence_;
    const auto result = GetHAL().enqueueMeetingOpusFrame(frame);
    if (result.enqueued || result.locally_accepted) {
        last_enqueued_sequence_ = next_sequence_;
        ++next_sequence_;
        packet.reset();
    } else {
        const int64_t now_us = esp_timer_get_time();
        if (last_diag_enqueue_fail_us_ == 0 || now_us - last_diag_enqueue_fail_us_ >= kDiagEnqueueFailIntervalUs) {
            last_diag_enqueue_fail_us_ = now_us;
            ESP_LOGW(
                TAG,
                "[SCMEET-DIAG] audio.enqueue_failed seq=%u error=%d pending=%u",
                next_sequence_,
                static_cast<int>(result.error),
                static_cast<unsigned>(pending_packets_.size())
            );
        }
    }
    return result;
}

MeetingEnqueueDisposition_t MeetingAudioBridge::enqueueControl(
    const char* action,
    const std::string& command_id,
    std::optional<uint32_t> sequence,
    const char* reason
) {
    ArduinoJson::JsonDocument doc;
    doc["protocolVersion"] = 1;
    doc["action"] = action;
    doc["messageId"] = createMessageId();
    doc["commandId"] = command_id;
    std::string session_id;
    {
        std::lock_guard<std::mutex> lock(mutex_);
        session_id = session_id_;
        doc["sessionId"] = session_id;
    }
    if (std::string_view(action) == "meeting.started") {
        doc["firstSequence"] = 0;
    } else if (sequence.has_value()) {
        doc["lastSequence"] = *sequence;
    } else {
        doc["lastSequence"] = nullptr;
    }
    if (reason != nullptr) {
        doc["reason"] = reason;
    }

    std::string json;
    ArduinoJson::serializeJson(doc, json);
    const auto disposition = GetHAL().enqueueMeetingControl(json);
    char seq_buf[16];
    ESP_LOGI(
        TAG,
        "[SCMEET-DIAG] control.enqueue action=%s lastSequence=%s enqueued=%d accepted=%d",
        action,
        FormatOptionalU32(sequence, seq_buf),
        disposition.enqueued ? 1 : 0,
        disposition.locally_accepted ? 1 : 0
    );
    rememberOutcome(
        std::string_view(action) == "meeting.started"
            ? BridgeOutcomeKind::Started
            : BridgeOutcomeKind::Stopped,
        session_id,
        command_id,
        json,
        disposition
    );
    return disposition;
}

void MeetingAudioBridge::rememberOutcome(
    BridgeOutcomeKind kind,
    const std::string& session_id,
    const std::string& command_id,
    const std::string& json,
    const MeetingEnqueueDisposition_t& disposition
) {
    std::lock_guard<std::mutex> lock(mutex_);
    for (auto& cached : control_outcomes_) {
        if (cached.kind == kind && cached.session_id == session_id &&
            cached.command_id == command_id) {
            cached.json = json;
            cached.disposition = disposition;
            return;
        }
    }
    if (control_outcomes_.size() >= kMaxCachedControlOutcomes) {
        control_outcomes_.pop_front();
    }
    control_outcomes_.push_back({kind, session_id, command_id, json, disposition});
}

bool MeetingAudioBridge::waitForTransport(uint32_t sequence, uint32_t timeout_ms)
{
    const int64_t deadline_us = esp_timer_get_time() + static_cast<int64_t>(timeout_ms) * 1000;
    while (true) {
        GetHAL().serviceMeetingTransport();
        const auto transport = GetHAL().getMeetingTransportSnapshot();
        if (transport.failureLatched) {
            return false;
        }
        if (transport.hasLocalSocketAcceptedSequence &&
            transport.lastLocalSocketAcceptedSequence == sequence &&
            transport.queuedFrames == 0) {
            return true;
        }
        if (esp_timer_get_time() >= deadline_us) {
            return false;
        }
        vTaskDelay(pdMS_TO_TICKS(10));
    }
}

bool MeetingAudioBridge::waitForControlFlush(uint64_t ticket, uint32_t timeout_ms)
{
    const int64_t deadline_us = esp_timer_get_time() + static_cast<int64_t>(timeout_ms) * 1000;
    while (true) {
        GetHAL().serviceMeetingTransport();
        const auto transport = GetHAL().getMeetingTransportSnapshot();
        if (transport.failureLatched) {
            return false;
        }
        if (transport.lastLocalSocketAcceptedTicket >= ticket) {
            return true;
        }
        if (esp_timer_get_time() >= deadline_us) {
            return false;
        }
        vTaskDelay(pdMS_TO_TICKS(10));
    }
}

}  // namespace stackchan::meeting
