#ifndef STACKCHAN_MEETING_SERVICE_LIFECYCLE_POLICY_H
#define STACKCHAN_MEETING_SERVICE_LIFECYCLE_POLICY_H

enum class MeetingServiceLifecycleState {
    Stopped = 0,
    Starting,
    Ready,
    Stopping,
};

class MeetingServiceLifecyclePolicy {
public:
    bool BeginEnsure()
    {
        if (state_ != MeetingServiceLifecycleState::Stopped) return false;
        state_ = MeetingServiceLifecycleState::Starting;
        return true;
    }

    void PublishReady()
    {
        if (state_ == MeetingServiceLifecycleState::Starting) {
            state_ = MeetingServiceLifecycleState::Ready;
        }
    }

    bool AcquireReady() const { return state_ == MeetingServiceLifecycleState::Ready; }

    void BeginStop()
    {
        if (state_ == MeetingServiceLifecycleState::Starting ||
            state_ == MeetingServiceLifecycleState::Ready) {
            state_ = MeetingServiceLifecycleState::Stopping;
        }
    }

    void PublishStopped() { state_ = MeetingServiceLifecycleState::Stopped; }
    MeetingServiceLifecycleState State() const { return state_; }

private:
    MeetingServiceLifecycleState state_ = MeetingServiceLifecycleState::Stopped;
};

#endif
