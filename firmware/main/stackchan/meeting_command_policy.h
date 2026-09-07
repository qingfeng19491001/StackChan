#ifndef STACKCHAN_MEETING_COMMAND_POLICY_H
#define STACKCHAN_MEETING_COMMAND_POLICY_H

enum class MeetingCommandDecision {
    Accept = 0,
    Duplicate,
    Conflict,
};

struct MeetingCommandKey {
    unsigned long long high = 0;
    unsigned long long low = 0;
};

constexpr bool operator==(MeetingCommandKey left, MeetingCommandKey right)
{
    return left.high == right.high && left.low == right.low;
}

constexpr bool operator!=(MeetingCommandKey left, MeetingCommandKey right)
{
    return !(left == right);
}

constexpr unsigned int MeetingHexNibble(char value)
{
    if (value >= '0' && value <= '9') return static_cast<unsigned int>(value - '0');
    if (value >= 'a' && value <= 'f') return static_cast<unsigned int>(value - 'a' + 10);
    if (value >= 'A' && value <= 'F') return static_cast<unsigned int>(value - 'A' + 10);
    return 0;
}

constexpr MeetingCommandKey MeetingCommandKeyFromUuid(const char* value)
{
    MeetingCommandKey result;
    unsigned int nibble_index = 0;
    if (value == nullptr) return result;
    while (*value != '\0' && nibble_index < 32) {
        if (*value != '-') {
            auto& half = nibble_index < 16 ? result.high : result.low;
            half = (half << 4U) | MeetingHexNibble(*value);
            ++nibble_index;
        }
        ++value;
    }
    return result;
}

constexpr bool IsCorrelatedMeetingError(
    bool active,
    MeetingCommandKey active_session,
    MeetingCommandKey start_command,
    bool stop_seen,
    MeetingCommandKey stop_command,
    bool error_session_present,
    MeetingCommandKey error_session,
    bool error_command_present,
    MeetingCommandKey error_command
) {
    return active && error_session_present && error_command_present &&
        error_session == active_session &&
        (error_command == start_command || (stop_seen && error_command == stop_command));
}

class MeetingCommandPolicy {
public:
    MeetingCommandDecision OnStart(MeetingCommandKey session, MeetingCommandKey command)
    {
        if (active_) {
            return active_session_ == session && start_command_ == command
                ? MeetingCommandDecision::Duplicate
                : MeetingCommandDecision::Conflict;
        }
        for (unsigned int index = 0; index < history_count_; ++index) {
            if (history_[index].session == session) {
                return history_[index].start_command == command
                    ? MeetingCommandDecision::Duplicate
                    : MeetingCommandDecision::Conflict;
            }
        }
        active_ = true;
        active_session_ = session;
        start_command_ = command;
        stop_seen_ = false;
        return MeetingCommandDecision::Accept;
    }

    MeetingCommandDecision OnStop(MeetingCommandKey session, MeetingCommandKey command)
    {
        if (!active_) {
            for (unsigned int index = 0; index < history_count_; ++index) {
                if (history_[index].session == session) {
                    return history_[index].stop_known && history_[index].stop_command == command
                        ? MeetingCommandDecision::Duplicate
                        : MeetingCommandDecision::Conflict;
                }
            }
            return MeetingCommandDecision::Conflict;
        }
        if (active_session_ != session) {
            return MeetingCommandDecision::Conflict;
        }
        if (stop_seen_) {
            return stop_command_ == command
                ? MeetingCommandDecision::Duplicate
                : MeetingCommandDecision::Conflict;
        }
        stop_command_ = command;
        stop_seen_ = true;
        return MeetingCommandDecision::Accept;
    }

    void MarkStopped(MeetingCommandKey session, MeetingCommandKey command)
    {
        if (!active_ || active_session_ != session || !stop_seen_ || stop_command_ != command) return;
        remember({active_session_, start_command_, stop_command_, true});
        active_ = false;
    }

    void MarkStartFailed(MeetingCommandKey session, MeetingCommandKey command)
    {
        if (!active_ || active_session_ != session || start_command_ != command) return;
        remember({active_session_, start_command_, {}, false});
        active_ = false;
        stop_seen_ = false;
    }

    void Abort()
    {
        active_ = false;
        stop_seen_ = false;
    }

private:
    struct CompletedCommand {
        MeetingCommandKey session;
        MeetingCommandKey start_command;
        MeetingCommandKey stop_command;
        bool stop_known = false;
    };

    void remember(const CompletedCommand& completed)
    {
        history_[history_next_] = completed;
        history_next_ = (history_next_ + 1U) % kHistorySize;
        if (history_count_ < kHistorySize) ++history_count_;
    }

    static constexpr unsigned int kHistorySize = 8;
    bool active_ = false;
    MeetingCommandKey active_session_;
    MeetingCommandKey start_command_;
    MeetingCommandKey stop_command_;
    bool stop_seen_ = false;
    CompletedCommand history_[kHistorySize]{};
    unsigned int history_count_ = 0;
    unsigned int history_next_ = 0;
};

#endif
