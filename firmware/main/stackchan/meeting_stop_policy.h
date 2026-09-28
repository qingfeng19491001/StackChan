#ifndef STACKCHAN_MEETING_STOP_POLICY_H
#define STACKCHAN_MEETING_STOP_POLICY_H

enum class MeetingIncompleteTerminalDisposition {
    IncompleteAudioReported = 0,
    TransportFailed,
};

constexpr MeetingIncompleteTerminalDisposition ResolveIncompleteTerminal(
    bool control_enqueued,
    bool control_flushed
) {
    return control_enqueued && control_flushed
        ? MeetingIncompleteTerminalDisposition::IncompleteAudioReported
        : MeetingIncompleteTerminalDisposition::TransportFailed;
}

constexpr bool ShouldCommitStoppedCommand(bool terminal_outcome_known)
{
    return terminal_outcome_known;
}

// App finishing (transcript/summary) does not require a perfect Opus drain.
// After an inbound meeting.stop, return Ready whenever a terminal
// meeting.stopped was already emitted, or the bridge is already idle.
// Latch ERROR only when the stop never reached a terminal control.
constexpr bool ShouldReturnReadyAfterStop(bool terminal_outcome_known, bool completed_ok)
{
    return terminal_outcome_known || completed_ok;
}

constexpr bool ShouldReportStopAsDeviceError(bool audio_running, bool terminal_outcome_known, bool completed_ok)
{
    if (!audio_running) return false;
    return !ShouldReturnReadyAfterStop(terminal_outcome_known, completed_ok);
}

#endif
