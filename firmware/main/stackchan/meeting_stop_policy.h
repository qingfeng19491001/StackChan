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

#endif
