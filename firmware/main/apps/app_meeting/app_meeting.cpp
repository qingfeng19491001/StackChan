/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "app_meeting.h"

#include <apps/common/common.h>
#include <assets/assets.h>
#include <hal/hal.h>
#include <mooncake_log.h>
#include <ArduinoJson.hpp>
#include <esp_system.h>
#include <stackchan/meeting_stop_policy.h>
#include <stackchan/meeting_app_failure_policy.h>

using namespace mooncake;

AppMeeting::AppMeeting()
{
    setAppInfo().name = "MEETING";

    // Phase 0 reuses an existing bundled icon. A dedicated asset can replace it
    // without changing the launcher registration or Meeting UI lifecycle.
    static auto icon  = assets::get_image("icon_ezdata.bin");
    setAppInfo().icon = (void*)&icon;

    static uint32_t theme_color = 0x31C7B5;
    setAppInfo().userData       = (void*)&theme_color;
}

void AppMeeting::onCreate()
{
    mclog::tagInfo(getAppInfo().name, "on create");

    _meeting_command_connection = GetHAL().onWsMeetingCommand.connect(
        [this](const WsMeetingCommand_t& command) { handleMeetingCommand(command); }
    );
    _connection_state_connection = GetHAL().onWsConnectionChanged.connect([this](bool connected) {
        std::lock_guard<std::mutex> lock(_command_mutex);
        _connected = connected;
        if (!connected) {
            _protocol_selected = false;
        }
    });
    _protocol_state_connection = GetHAL().onWsMeetingProtocolSelected.connect([this](bool selected) {
        std::lock_guard<std::mutex> lock(_command_mutex);
        _protocol_selected = selected;
    });
    _meeting_event_connection = GetHAL().onMeetingEvent.connect(
        [this](const MeetingEvent_t& event) { handleMeetingEvent(event); }
    );
}

void AppMeeting::onOpen()
{
    mclog::tagInfo(getAppInfo().name, "on open");

    GetHAL().ensureWebSocketAvatarServiceStarted();
    const auto transport = GetHAL().getMeetingTransportSnapshot();
    {
        std::lock_guard<std::mutex> state_lock(_command_mutex);
        _connected = transport.connected;
        _protocol_selected = transport.protocolSelected;
        _ui_open = true;
        _next_pairing_attempt_ms = 0;
        _pairing_uri.clear();
    }

    LvglLockGuard lock;
    _page = std::make_unique<view::MeetingPage>();
    _page->onAction([this]() { handleLocalAction(); });
    _page->setState(_meeting_error_latched.load()
                        ? view::MeetingUiState::Error
                        : (transport.connected
                               ? view::MeetingUiState::Ready
                               : view::MeetingUiState::Disconnected));

    view::create_home_indicator([this]() {
        if (_audio_bridge.isRunning()) {
            requestLocalStop();
        } else {
            close();
        }
    }, 0x75E4D8, 0x082E31);
    view::create_status_bar(0x75E4D8, 0x082E31);
}

void AppMeeting::onRunning()
{
    processPendingCommands();

    bool connected = false;
    bool selected = false;
    bool awaiting_start_offer = false;
    bool event_pending = false;
    {
        std::lock_guard<std::mutex> state_lock(_command_mutex);
        connected = _connected;
        selected = _protocol_selected;
        awaiting_start_offer = _awaiting_start_offer;
        event_pending = _meeting_event_pending;
        _meeting_event_pending = false;
    }
    std::optional<stackchan::meeting::BridgeHealthSnapshot> transport_health;
    if (!event_pending && _audio_bridge.isRunning()) {
        transport_health = _audio_bridge.inspectTransportHealth();
    }
    const auto failure_action = _failure_state.Observe(
        event_pending,
        transport_health.has_value() && transport_health->failure_latched
    );
    if (event_pending) {
        WsMeetingCommand_t failed;
        failed.action = MeetingInboundAction::Start;
        failed.commandId = _active_command_id;
        failed.sessionId = _active_session_id;
        stackchan::meeting::BridgeAbortResult aborted;
        if (_audio_bridge.isRunning()) {
            aborted = _audio_bridge.abort(stackchan::meeting::MeetingError::ProtocolRejected);
        }
        if (!failed.sessionId.empty() && !failed.commandId.empty()) {
            aborted.error_control = sendError(failed, "DEVICE_ERROR", &aborted);
            markStartFailed(failed.sessionId, failed.commandId);
        } else {
            GetHAL().abortMeetingCommandState();
            std::lock_guard<std::mutex> state_lock(_command_mutex);
            _command_policy.Abort();
        }
        {
            std::lock_guard<std::mutex> state_lock(_command_mutex);
            _last_abort_result = aborted;
            _awaiting_start_offer = false;
        }
        _meeting_error_latched.store(true);
        updateUiState(view::MeetingUiState::Error);
    } else if (failure_action == MeetingAppFailureAction::AbortToError) {
        const auto& health = *transport_health;
        WsMeetingCommand_t failed;
        failed.action = MeetingInboundAction::Stop;
        failed.commandId = _active_command_id;
        failed.sessionId = _active_session_id;
        auto aborted = _audio_bridge.abort(health.error);
        _meeting_error_latched.store(true);
        const char* code = health.error == stackchan::meeting::MeetingError::TransportUnavailable
            ? "SERVER_DISCONNECTED"
            : "WRITE_FAILED";
        aborted.error_control = sendError(failed, code, &aborted);
        markStartFailed(failed.sessionId, failed.commandId);
        {
            std::lock_guard<std::mutex> state_lock(_command_mutex);
            _last_abort_result = aborted;
        }
        updateUiState(view::MeetingUiState::Error);
    } else if (_page && !awaiting_start_offer && !_meeting_error_latched.load()) {
        // Pairing is an HTTP capability of an authenticated, online device.
        // It must not wait for meeting-v1 negotiation: that negotiation is
        // completed by the Auro WebSocket after the user scans this QR code.
        updateUiState(connected ? view::MeetingUiState::Ready
                                : view::MeetingUiState::Disconnected);
    }
    refreshPairingQrIfNeeded(connected, selected);

    LvglLockGuard lock;
    view::update_home_indicator();
    view::update_status_bar();
}

void AppMeeting::onClose()
{
    mclog::tagInfo(getAppInfo().name, "on close");

    const bool had_active_meeting = _audio_bridge.isRunning();
    _audio_bridge.abort();
    if (had_active_meeting && !_active_session_id.empty() && !_active_command_id.empty()) {
        WsMeetingCommand_t failed;
        failed.action = MeetingInboundAction::Start;
        failed.sessionId = _active_session_id;
        failed.commandId = _active_command_id;
        sendError(failed, "DEVICE_ERROR");
        markStartFailed(failed.sessionId, failed.commandId);
    } else {
        GetHAL().abortMeetingCommandState();
    }
    {
        std::lock_guard<std::mutex> state_lock(_command_mutex);
        _ui_open = false;
        _pairing_uri.clear();
    }

    LvglLockGuard lock;
    _page.reset();
    view::destroy_home_indicator();
    view::destroy_status_bar();
}

void AppMeeting::onDestroy()
{
    _audio_bridge.abort();
    GetHAL().abortMeetingCommandState();
    GetHAL().onWsMeetingCommand.disconnect(_meeting_command_connection);
    GetHAL().onWsConnectionChanged.disconnect(_connection_state_connection);
    GetHAL().onWsMeetingProtocolSelected.disconnect(_protocol_state_connection);
    GetHAL().onMeetingEvent.disconnect(_meeting_event_connection);
}

void AppMeeting::handleMeetingCommand(const WsMeetingCommand_t& command)
{
    if (command.action != MeetingInboundAction::Start && command.action != MeetingInboundAction::Stop) {
        return;
    }
    if (command.commandId.empty() || command.sessionId.empty()) {
        sendError(command, "DEVICE_ERROR");
        return;
    }

    MeetingCommandDecision decision = MeetingCommandDecision::Conflict;
    {
        std::lock_guard<std::mutex> lock(_command_mutex);
        const auto session = MeetingCommandKeyFromUuid(command.sessionId.c_str());
        const auto command_key = MeetingCommandKeyFromUuid(command.commandId.c_str());
        decision = command.action == MeetingInboundAction::Start
            ? _command_policy.OnStart(session, command_key)
            : _command_policy.OnStop(session, command_key);
        if (decision == MeetingCommandDecision::Duplicate) {
            // Replayed Server commands are handled outside the mutex.
        } else if (decision == MeetingCommandDecision::Conflict) {
            // Emit outside the mutex because enqueueing may synchronously use transport locks.
        } else if (command.action == MeetingInboundAction::Start) {
            _pending_start = command;
            _awaiting_start_offer = false;
        } else {
            _pending_stop = command;
        }
    }

    if (decision == MeetingCommandDecision::Duplicate) {
        auto replay = command.action == MeetingInboundAction::Start
            ? _audio_bridge.replayOutcome(
                  stackchan::meeting::BridgeOutcomeKind::Error,
                  command.sessionId,
                  command.commandId
              )
            : stackchan::meeting::BridgeReplayResult{};
        if (!replay.found) {
            const auto kind = command.action == MeetingInboundAction::Start
                ? stackchan::meeting::BridgeOutcomeKind::Started
                : stackchan::meeting::BridgeOutcomeKind::Stopped;
            replay = _audio_bridge.replayOutcome(kind, command.sessionId, command.commandId);
        }
        if (!replay.found && command.action == MeetingInboundAction::Stop) {
            replay = _audio_bridge.replayOutcome(
                stackchan::meeting::BridgeOutcomeKind::Error,
                command.sessionId,
                command.commandId
            );
        }
        if (!replay.found) {
            // A remotely failed start can have no device-authored response;
            // preserving the terminal identity is safer than a divergent ACK.
            return;
        }
        if (command.action == MeetingInboundAction::Stop && replay.disposition.locally_accepted) {
            {
                std::lock_guard<std::mutex> command_lock(_command_mutex);
                _command_policy.MarkStopped(
                    MeetingCommandKeyFromUuid(command.sessionId.c_str()),
                    MeetingCommandKeyFromUuid(command.commandId.c_str())
                );
            }
            GetHAL().markMeetingCommandStopped(command.sessionId, command.commandId);
        }
        return;
    }

    if (decision == MeetingCommandDecision::Conflict) {
        sendError(command, "SESSION_BUSY");
        return;
    }

    if (command.action == MeetingInboundAction::Start) {
        open();
    }
}

void AppMeeting::handleMeetingEvent(const MeetingEvent_t& event)
{
    // Rejections describe the offending inbound control and are reported to
    // the Server by HAL; they never mutate the current meeting lifecycle.
    if (event.kind == MeetingEventKind::ControlRejected ||
        event.kind == MeetingEventKind::TransportFailure) return;
    std::lock_guard<std::mutex> lock(_command_mutex);
    _meeting_event_pending = true;
    _meeting_error_latched.store(true);
}

void AppMeeting::processPendingCommands()
{
    std::optional<WsMeetingCommand_t> start_command;
    std::optional<WsMeetingCommand_t> stop_command;
    {
        std::lock_guard<std::mutex> lock(_command_mutex);
        start_command = std::move(_pending_start);
        stop_command = std::move(_pending_stop);
        _pending_start.reset();
        _pending_stop.reset();
    }

    if (start_command.has_value()) {
        if (_audio_bridge.isRunning()) {
            if (_active_session_id != start_command->sessionId) {
                sendError(*start_command, "SESSION_BUSY");
            }
        } else {
            updateUiState(view::MeetingUiState::Preparing);
            const auto prepared = _audio_bridge.prepare();
            if (prepared != stackchan::meeting::BridgePrepareResult::Ready &&
                prepared != stackchan::meeting::BridgePrepareResult::AlreadyReady) {
                {
                    std::lock_guard<std::mutex> command_lock(_command_mutex);
                    _command_policy.MarkStartFailed(
                        MeetingCommandKeyFromUuid(start_command->sessionId.c_str()),
                        MeetingCommandKeyFromUuid(start_command->commandId.c_str())
                    );
                }
                _meeting_error_latched.store(true);
                sendError(*start_command, "DEVICE_ERROR");
                GetHAL().markMeetingCommandStartFailed(
                    start_command->sessionId, start_command->commandId
                );
                updateUiState(view::MeetingUiState::Error);
            } else {
                const auto started = _audio_bridge.start(
                    start_command->sessionId, start_command->commandId
                );
                if (started == stackchan::meeting::BridgeStartResult::Started) {
                    _failure_state.OnAcceptedStart();
                    _meeting_error_latched.store(false);
                    _active_session_id = start_command->sessionId;
                    _active_command_id = start_command->commandId;
                    updateUiState(view::MeetingUiState::Recording);
                } else {
                    {
                        std::lock_guard<std::mutex> command_lock(_command_mutex);
                        _command_policy.MarkStartFailed(
                            MeetingCommandKeyFromUuid(start_command->sessionId.c_str()),
                            MeetingCommandKeyFromUuid(start_command->commandId.c_str())
                        );
                    }
                    _meeting_error_latched.store(true);
                    sendError(*start_command, "DEVICE_ERROR");
                    GetHAL().markMeetingCommandStartFailed(
                        start_command->sessionId, start_command->commandId
                    );
                    updateUiState(view::MeetingUiState::Error);
                }
            }
        }
    }

    if (stop_command.has_value()) {
        if (!_audio_bridge.isRunning() || stop_command->sessionId != _active_session_id) {
            sendError(*stop_command, "DEVICE_ERROR");
            return;
        }
        updateUiState(view::MeetingUiState::Stopping);
        auto result = _audio_bridge.stopAndDrain(stop_command->commandId);
        if (ShouldCommitStoppedCommand(result.terminal_outcome_known)) {
            {
                std::lock_guard<std::mutex> command_lock(_command_mutex);
                _command_policy.MarkStopped(
                    MeetingCommandKeyFromUuid(stop_command->sessionId.c_str()),
                    MeetingCommandKeyFromUuid(stop_command->commandId.c_str())
                );
            }
            GetHAL().markMeetingCommandStopped(stop_command->sessionId, stop_command->commandId);
        }
        if (result.completed && result.status == stackchan::meeting::BridgeStopStatus::Complete &&
            result.error == stackchan::meeting::MeetingError::None) {
            _active_session_id.clear();
            _active_command_id.clear();
            updateUiState(view::MeetingUiState::Ready);
        } else {
            _meeting_error_latched.store(true);
            stackchan::meeting::BridgeAbortResult evidence;
            evidence.cause = result.error;
            evidence.last_attempted_sequence = result.last_attempted_sequence;
            evidence.last_enqueued_sequence = result.last_enqueued_sequence;
            evidence.last_transport_accepted_sequence = result.last_transport_accepted_sequence;
            result.error_control = sendError(*stop_command, "DEVICE_ERROR", &evidence);
            updateUiState(view::MeetingUiState::Error);
        }
        {
            std::lock_guard<std::mutex> command_lock(_command_mutex);
            _last_stop_result = result;
        }
    }
}

void AppMeeting::handleLocalAction()
{
    if (_audio_bridge.isRunning()) {
        requestLocalStop();
    } else {
        requestLocalStart();
    }
}

void AppMeeting::requestLocalStart()
{
    const auto transport = GetHAL().getMeetingTransportSnapshot();
    if (!transport.connected || !transport.protocolSelected) {
        updateUiState(view::MeetingUiState::Disconnected);
        return;
    }

    const auto session_id = fmt::format(
        "{:08x}-{:04x}-4{:03x}-{:04x}-{:08x}{:04x}",
        esp_random(), esp_random() & 0xFFFFU, esp_random() & 0x0FFFU,
        (esp_random() & 0x3FFFU) | 0x8000U, esp_random(), esp_random() & 0xFFFFU
    );
    const auto command_id = fmt::format(
        "{:08x}-{:04x}-4{:03x}-{:04x}-{:08x}{:04x}",
        esp_random(), esp_random() & 0xFFFFU, esp_random() & 0x0FFFU,
        (esp_random() & 0x3FFFU) | 0x8000U, esp_random(), esp_random() & 0xFFFFU
    );

    ArduinoJson::JsonDocument doc;
    doc["protocolVersion"] = 1;
    doc["action"] = "meeting.start-requested";
    doc["messageId"] = command_id;
    doc["commandId"] = command_id;
    doc["sessionId"] = session_id;
    std::string json;
    ArduinoJson::serializeJson(doc, json);
    const auto disposition = GetHAL().enqueueMeetingControl(json);
    if (disposition.enqueued || disposition.locally_accepted) {
        {
            std::lock_guard<std::mutex> lock(_command_mutex);
            _awaiting_start_offer = true;
        }
        updateUiState(view::MeetingUiState::Preparing);
    } else {
        updateUiState(view::MeetingUiState::Error);
    }
}

void AppMeeting::requestLocalStop()
{
    if (!_audio_bridge.isRunning() || _active_session_id.empty()) {
        return;
    }
    const auto command_id = fmt::format(
        "{:08x}-{:04x}-4{:03x}-{:04x}-{:08x}{:04x}",
        esp_random(), esp_random() & 0xFFFFU, esp_random() & 0x0FFFU,
        (esp_random() & 0x3FFFU) | 0x8000U, esp_random(), esp_random() & 0xFFFFU
    );
    ArduinoJson::JsonDocument doc;
    doc["protocolVersion"] = 1;
    doc["action"] = "meeting.stop-requested";
    doc["messageId"] = command_id;
    doc["commandId"] = command_id;
    doc["sessionId"] = _active_session_id;
    doc["reason"] = "user";
    std::string json;
    ArduinoJson::serializeJson(doc, json);
    const auto disposition = GetHAL().enqueueMeetingControl(json);
    if (disposition.enqueued || disposition.locally_accepted) {
        updateUiState(view::MeetingUiState::Stopping);
    } else {
        updateUiState(view::MeetingUiState::Error);
    }
}

MeetingEnqueueDisposition_t AppMeeting::sendError(
    const WsMeetingCommand_t& command,
    const char* code,
    const stackchan::meeting::BridgeAbortResult* evidence
)
{
    ArduinoJson::JsonDocument doc;
    doc["protocolVersion"] = 1;
    doc["action"] = "meeting.error";
    doc["messageId"] = fmt::format(
        "{:08x}-{:04x}-4{:03x}-{:04x}-{:08x}{:04x}",
        esp_random(), esp_random() & 0xFFFFU, esp_random() & 0x0FFFU,
        (esp_random() & 0x3FFFU) | 0x8000U, esp_random(), esp_random() & 0xFFFFU
    );
    if (!command.messageId.empty()) doc["correlationMessageId"] = command.messageId;
    if (!command.commandId.empty()) doc["commandId"] = command.commandId;
    if (!command.sessionId.empty()) doc["sessionId"] = command.sessionId;
    doc["code"] = code;
    if (evidence != nullptr) {
        if (evidence->last_attempted_sequence.has_value()) {
            doc["lastAttemptedSequence"] = *evidence->last_attempted_sequence;
        }
        if (evidence->last_enqueued_sequence.has_value()) {
            doc["lastEnqueuedSequence"] = *evidence->last_enqueued_sequence;
        }
        if (evidence->last_transport_accepted_sequence.has_value()) {
            doc["lastLocalSocketAcceptedSequence"] =
                *evidence->last_transport_accepted_sequence;
        }
    }
    std::string json;
    ArduinoJson::serializeJson(doc, json);
    if (!command.sessionId.empty() && !command.commandId.empty()) {
        return _audio_bridge.enqueueErrorOutcome(command.sessionId, command.commandId, json);
    }
    return GetHAL().enqueueMeetingControl(json);
}

void AppMeeting::markStartFailed(
    const std::string& session_id,
    const std::string& command_id
)
{
    {
        std::lock_guard<std::mutex> command_lock(_command_mutex);
        _command_policy.MarkStartFailed(
            MeetingCommandKeyFromUuid(session_id.c_str()),
            MeetingCommandKeyFromUuid(command_id.c_str())
        );
    }
    GetHAL().markMeetingCommandStartFailed(session_id, command_id);
}

void AppMeeting::updateUiState(view::MeetingUiState state)
{
    if (_page == nullptr) {
        return;
    }
    LvglLockGuard lock;
    _page->setState(state);
}

void AppMeeting::refreshPairingQrIfNeeded(bool connected, bool protocol_selected)
{
    // Capability negotiation gates meeting controls, not QR pairing. Requiring
    // it here would deadlock first-time binding before Auro can open its socket.
    (void)protocol_selected;
    bool ui_open = false;
    bool awaiting_start_offer = false;
    {
        std::lock_guard<std::mutex> state_lock(_command_mutex);
        ui_open = _ui_open;
        awaiting_start_offer = _awaiting_start_offer;
    }
    if (!connected || _audio_bridge.isRunning() || awaiting_start_offer ||
        !ui_open || _page == nullptr) {
        if (!connected && !_pairing_uri.empty() && _page != nullptr) {
            _pairing_uri.clear();
            LvglLockGuard lock;
            _page->setPairingQr({});
        }
        return;
    }

    const uint32_t now = GetHAL().millis();
    if (_next_pairing_attempt_ms != 0 && static_cast<int32_t>(now - _next_pairing_attempt_ms) < 0) {
        return;
    }

    const auto pairing = GetHAL().requestMeetingPairingNonce();
    if (pairing.status == MeetingPairingStatus::Ready) {
        _pairing_uri = pairing.pairUri;
        // Server nonces currently live for two minutes. Refresh before expiry
        // without depending on the device wall clock being synchronized.
        _next_pairing_attempt_ms = now + 90'000;
        LvglLockGuard lock;
        _page->setPairingQr(_pairing_uri);
        return;
    }

    _next_pairing_attempt_ms = now + 5'000;
    if (pairing.status == MeetingPairingStatus::DeviceOffline ||
        pairing.status == MeetingPairingStatus::NetworkUnavailable) {
        updateUiState(view::MeetingUiState::Reconnecting);
    } else if (pairing.status == MeetingPairingStatus::MissingCredential ||
               pairing.status == MeetingPairingStatus::Unauthorized ||
               pairing.status == MeetingPairingStatus::ProtocolUnsupported) {
        updateUiState(view::MeetingUiState::Error);
    }
}
