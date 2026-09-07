#ifndef STACKCHAN_MEETING_OUTCOME_REPLAY_POLICY_H
#define STACKCHAN_MEETING_OUTCOME_REPLAY_POLICY_H

enum class MeetingOutcomeReplayAction {
    Missing = 0,
    KeepPending,
    ReplayExactBytes,
    TerminalFailure,
};

constexpr MeetingOutcomeReplayAction ResolveMeetingOutcomeReplay(
    bool has_cached_outcome,
    unsigned long long ticket,
    bool enqueued,
    unsigned long long last_local_socket_accepted_ticket,
    bool failure_latched
) {
    if (!has_cached_outcome) return MeetingOutcomeReplayAction::Missing;
    if (failure_latched) return MeetingOutcomeReplayAction::TerminalFailure;
    if (ticket != 0 && enqueued && last_local_socket_accepted_ticket < ticket) {
        return MeetingOutcomeReplayAction::KeepPending;
    }
    return MeetingOutcomeReplayAction::ReplayExactBytes;
}

#endif
