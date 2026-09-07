#ifndef STACKCHAN_MEETING_APP_FAILURE_POLICY_H
#define STACKCHAN_MEETING_APP_FAILURE_POLICY_H

enum class MeetingAppFailureAction {
    Continue = 0,
    AbortToError,
};

constexpr MeetingAppFailureAction ResolveMeetingAppFailure(
    bool event_pending,
    bool transport_failure_latched
) {
    return event_pending || transport_failure_latched
        ? MeetingAppFailureAction::AbortToError
        : MeetingAppFailureAction::Continue;
}

class MeetingAppFailureState {
public:
    MeetingAppFailureAction Observe(bool event_pending, bool transport_failure_latched)
    {
        const auto action = ResolveMeetingAppFailure(event_pending, transport_failure_latched);
        if (action == MeetingAppFailureAction::AbortToError) error_latched_ = true;
        return action;
    }

    void OnAcceptedStart() { error_latched_ = false; }
    bool ErrorLatched() const { return error_latched_; }

private:
    bool error_latched_ = false;
};

#endif
