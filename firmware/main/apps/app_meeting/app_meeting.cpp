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
#include <stackchan/meeting_ui_state_policy.h>

using namespace mooncake;

namespace {

view::MeetingUiState ToViewMeetingUiState(MeetingUiStateResolution state)
{
    switch (state) {
        case MeetingUiStateResolution::Disconnected: return view::MeetingUiState::Disconnected;
        case MeetingUiStateResolution::Reconnecting: return view::MeetingUiState::Reconnecting;
        case MeetingUiStateResolution::Ready: return view::MeetingUiState::Ready;
        case MeetingUiStateResolution::Preparing: return view::MeetingUiState::Preparing;
        case MeetingUiStateResolution::Recording: return view::MeetingUiState::Recording;
        case MeetingUiStateResolution::Stopping: return view::MeetingUiState::Stopping;
    }
    return view::MeetingUiState::Error;
}

constexpr uint32_t kLocalStopConfirmationTimeoutMs = 12'000;

}  // namespace

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

    // Audio processor initialization is intentionally completed before the
    // device WebSocket is started in app_main. Once the device is advertised
    // online, meeting.start must remain a short command/acknowledgement path.
    const auto prepared = _audio_bridge.prepare();
    if (prepared != stackchan::meeting::BridgePrepareResult::Ready &&
        prepared != stackchan::meeting::BridgePrepareResult::AlreadyReady) {
        _meeting_error_latched.store(true);
    }

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
                        : ToViewMeetingUiState(ResolveMeetingUiState(
                              transport.connected, transport.protocolSelected, _audio_bridge.isRunning(),
                              false, false
                          )));

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
    bool awaiting_local_stop = false;
    {
        std::lock_guard<std::mutex> state_lock(_command_mutex);
        connected = _connected;
        selected = _protocol_selected;
        awaiting_start_offer = _awaiting_start_offer;
        event_pending = _meeting_event_pending;
        _meeting_event_pending = false;
        if (_pending_local_stop.has_value()) {
            const uint32_t now = GetHAL().millis();
            if (static_cast<int32_t>(now - _pending_local_stop->deadline_ms) >= 0) {
                _pending_local_stop.reset();
            } else {
                awaiting_local_stop = true;
            }
        }
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
            aborted.error_control = sendError(failed, "DEVICE_ERROR");
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
        mclog::tagWarn(
            getAppInfo().name,
            "[SCMEET-DIAG] transport.abort code={} error={}",
            code,
            static_cast<int>(health.error)
        );
        aborted.error_control = sendError(failed, code);
        markStartFailed(failed.sessionId, failed.commandId);
        {
            std::lock_guard<std::mutex> state_lock(_command_mutex);
            _last_abort_result = aborted;
        }
        updateUiState(view::MeetingUiState::Error);
    } else if (_page && !_meeting_error_latched.load()) {
        // Pairing is an HTTP capability of an authenticated, online device.
        // It must not wait for meeting-v1 negotiation: that negotiation is
        // completed by the Auro WebSocket after the user scans this QR code.
        updateUiState(ToViewMeetingUiState(ResolveMeetingUiState(
            connected, selected, _audio_bridge.isRunning(), awaiting_start_offer, awaiting_local_stop
        )));
    }
    refreshPairingQrIfNeeded(connected, selected);

    LvglLockGuard lock;
    view::update_home_indicator();
    view::update_status_bar();
}

void AppMeeting::onClose()
{
    mclog::tagInfo(getAppInfo().name, "on close");

    // The meeting session belongs to the control protocol, not its LVGL page.
    // Closing the page while the phone backgrounds or changes view must not
    // turn a healthy recording into meeting.error. A running session is ended
    // only by meeting.stop (or by the explicit failure paths in onRunning()).
    if (!_audio_bridge.isRunning()) {
        _audio_bridge.abort();
        GetHAL().abortMeetingCommandState();
    }
    {
        std::lock_guard<std::mutex> state_lock(_command_mutex);
        _ui_open = false;
        _pairing_uri.clear();
        _pending_local_stop.reset();
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
        event.kind == MeetingEventKind::TransportFailure) {
        mclog::tagWarn(
            getAppInfo().name,
            "[SCMEET-DIAG] meeting.event kind={} ignored_for_lifecycle",
            static_cast<int>(event.kind)
        );
        return;
    }
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
                    mclog::tagInfo(
                        getAppInfo().name,
                        "[SCMEET-DIAG] start.accepted session={}",
                        start_command->sessionId.c_str()
                    );
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
                    mclog::tagWarn(
                        getAppInfo().name,
                        "[SCMEET-DIAG] start.failed session={} result={}",
                        start_command->sessionId.c_str(),
                        static_cast<int>(started)
                    );
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
        {
            std::lock_guard<std::mutex> command_lock(_command_mutex);
            if (_pending_local_stop.has_value() &&
                _pending_local_stop->session_id == stop_command->sessionId &&
                _pending_local_stop->command_id == stop_command->commandId) {
                _pending_local_stop.reset();
            }
        }
        if (!_audio_bridge.isRunning() || stop_command->sessionId != _active_session_id) {
            if (!_audio_bridge.isRunning()) {
                _active_session_id.clear();
                _active_command_id.clear();
                _meeting_error_latched.store(false);
                updateUiState(view::MeetingUiState::Ready);
                return;
            }
            sendError(*stop_command, "DEVICE_ERROR");
            return;
        }
        updateUiState(view::MeetingUiState::Stopping);
        auto result = _audio_bridge.stopAndDrain(stop_command->commandId);
        mclog::tagInfo(
            getAppInfo().name,
            "[SCMEET-DIAG] stop.drain status={} lastSequence={} accepted={} terminalKnown={}",
            static_cast<int>(result.status),
            result.last_sequence.has_value() ? static_cast<int>(*result.last_sequence) : -1,
            result.last_transport_accepted_sequence.has_value()
                ? static_cast<int>(*result.last_transport_accepted_sequence)
                : -1,
            result.terminal_outcome_known ? 1 : 0
        );
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
        const bool completed_ok =
            result.completed &&
            result.status == stackchan::meeting::BridgeStopStatus::Complete &&
            result.error == stackchan::meeting::MeetingError::None;
        if (!ShouldReportStopAsDeviceError(
                _audio_bridge.isRunning(), result.terminal_outcome_known, completed_ok
            )) {
            _active_session_id.clear();
            _active_command_id.clear();
            _meeting_error_latched.store(false);
            updateUiState(view::MeetingUiState::Ready);
        } else {
            _meeting_error_latched.store(true);
            result.error_control = sendError(*stop_command, "DEVICE_ERROR");
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
        {
            std::lock_guard<std::mutex> lock(_command_mutex);
            _pending_local_stop = PendingLocalStop{
                _active_session_id,
                command_id,
                GetHAL().millis() + kLocalStopConfirmationTimeoutMs,
            };
        }
        updateUiState(view::MeetingUiState::Stopping);
    } else {
        updateUiState(view::MeetingUiState::Error);
    }
}

MeetingEnqueueDisposition_t AppMeeting::sendError(
    const WsMeetingCommand_t& command,
    const char* code
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
    if (!command.commandId.empty()) doc["commandId"] = command.commandId;
    if (!command.sessionId.empty()) doc["sessionId"] = command.sessionId;
    doc["code"] = code;
    mclog::tagWarn(
        getAppInfo().name,
        "[SCMEET-DIAG] meeting.error code={} session={}",
        code,
        command.sessionId.c_str()
    );
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
