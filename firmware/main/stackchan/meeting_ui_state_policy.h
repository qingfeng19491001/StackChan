#ifndef STACKCHAN_MEETING_UI_STATE_POLICY_H
#define STACKCHAN_MEETING_UI_STATE_POLICY_H

enum class MeetingUiStateResolution {
    Disconnected = 0,
    Reconnecting,
    Ready,
    Preparing,
    Recording,
    Stopping,
};

constexpr MeetingUiStateResolution ResolveMeetingUiState(
    bool connected,
    bool protocol_selected,
    bool audio_running,
    bool awaiting_start_offer,
    bool awaiting_local_stop
) {
    if (awaiting_local_stop) return MeetingUiStateResolution::Stopping;
    if (audio_running) {
        return connected && protocol_selected
            ? MeetingUiStateResolution::Recording
            : MeetingUiStateResolution::Reconnecting;
    }
    if (awaiting_start_offer || (connected && !protocol_selected)) {
        return MeetingUiStateResolution::Preparing;
    }
    return connected
        ? MeetingUiStateResolution::Ready
        : MeetingUiStateResolution::Disconnected;
}

#endif
