/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once

#include "view/view.h"
#include <memory>
#include <atomic>
#include <mutex>
#include <optional>
#include <mooncake.h>
#include <hal/hal.h>
#include <stackchan/meeting_audio_bridge.h>
#include <stackchan/meeting_command_policy.h>
#include <stackchan/meeting_app_failure_policy.h>

class AppMeeting : public mooncake::AppAbility {
public:
    AppMeeting();

    void onCreate() override;
    void onOpen() override;
    void onRunning() override;
    void onClose() override;
    void onDestroy() override;

private:
    std::unique_ptr<view::MeetingPage> _page;
    stackchan::meeting::MeetingAudioBridge _audio_bridge;
    std::mutex _command_mutex;
    std::optional<WsMeetingCommand_t> _pending_start;
    std::optional<WsMeetingCommand_t> _pending_stop;
    std::string _active_session_id;
    std::string _active_command_id;
    size_t _meeting_command_connection = 0;
    size_t _connection_state_connection = 0;
    size_t _protocol_state_connection = 0;
    size_t _meeting_event_connection = 0;
    bool _connected = false;
    bool _protocol_selected = false;
    bool _ui_open = false;
    bool _awaiting_start_offer = false;
    uint32_t _next_pairing_attempt_ms = 0;
    std::string _pairing_uri;
    MeetingCommandPolicy _command_policy;
    std::atomic<bool> _meeting_error_latched{false};
    bool _meeting_event_pending = false;
    MeetingAppFailureState _failure_state;
    std::optional<stackchan::meeting::BridgeAbortResult> _last_abort_result;
    std::optional<stackchan::meeting::BridgeStopResult> _last_stop_result;

    void handleMeetingCommand(const WsMeetingCommand_t& command);
    void handleMeetingEvent(const MeetingEvent_t& event);
    void processPendingCommands();
    void handleLocalAction();
    void requestLocalStart();
    void requestLocalStop();
    MeetingEnqueueDisposition_t sendError(
        const WsMeetingCommand_t& command,
        const char* code,
        const stackchan::meeting::BridgeAbortResult* evidence = nullptr
    );
    void markStartFailed(const std::string& session_id, const std::string& command_id);
    void updateUiState(view::MeetingUiState state);
    void refreshPairingQrIfNeeded(bool connected, bool protocol_selected);
};
