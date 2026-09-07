#ifndef STACKCHAN_MEETING_TRANSPORT_POLICY_H
#define STACKCHAN_MEETING_TRANSPORT_POLICY_H

// "LocalSocketAccepted" means only that WebSocket::Send() returned true on
// the device. It is deliberately not a Server receipt or Auro consumption ACK.
// End-to-end completeness still requires the protocol's lastSequence
// reconciliation at Auro.
enum class MeetingMessageDisposition {
    Rejected = 0,
    Queued,
    LocalSocketAccepted,
    TerminalFailure,
};

enum class MeetingTransportError {
    None = 0,
    ServiceUnavailable,
    NotConnected,
    ProtocolNotSelected,
    InvalidPayload,
    QueueFull,
    SendFailed,
    PressureTimeout,
    ServiceStopping,
};

struct MeetingEnqueueDisposition_t {
    MeetingMessageDisposition disposition = MeetingMessageDisposition::Rejected;
    MeetingTransportError error = MeetingTransportError::None;
    unsigned long long ticket = 0;
    bool enqueued = false;
    bool locally_accepted = false;
};

struct MeetingTransportPolicySnapshot {
    bool connected = false;
    bool protocol_selected = false;
    unsigned long queued_messages = 0;
    unsigned long queued_bytes = 0;
    bool pressure_active = false;
    unsigned long long first_failure_ms = 0;
    bool failure_latched = false;
    MeetingTransportError error = MeetingTransportError::None;
    MeetingTransportError pressure_cause = MeetingTransportError::None;
    unsigned long long last_local_socket_accepted_ticket = 0;
    bool has_local_socket_accepted_sequence = false;
    unsigned int last_local_socket_accepted_sequence = 0;
};

class MeetingTransportPolicy {
public:
    MeetingTransportPolicy(
        unsigned long max_messages,
        unsigned long max_bytes,
        unsigned long long pressure_timeout_ms
    )
        : max_messages_(max_messages), max_bytes_(max_bytes), pressure_timeout_ms_(pressure_timeout_ms)
    {
    }

    void SetConnection(bool connected, unsigned long long now_ms)
    {
        connected_ = connected;
        if (!connected) {
            NoteFailure(MeetingTransportError::NotConnected, now_ms);
        } else if (!failure_latched_ && protocol_selected_ && queued_messages_ == 0) {
            ClearTransientFailure();
        }
    }

    void SetProtocolSelected(bool selected, unsigned long long now_ms)
    {
        protocol_selected_ = selected;
        if (!selected) {
            NoteFailure(MeetingTransportError::ProtocolNotSelected, now_ms);
        } else if (!failure_latched_ && connected_ && queued_messages_ == 0) {
            ClearTransientFailure();
        }
    }

    MeetingEnqueueDisposition_t Admit(
        unsigned long bytes,
        bool,
        unsigned int,
        unsigned long long now_ms
    ) {
        CheckDeadline(now_ms);
        if (failure_latched_) {
            return TerminalResult();
        }
        if (!connected_) {
            NoteFailure(MeetingTransportError::NotConnected, now_ms);
            return Rejected(MeetingTransportError::NotConnected);
        }
        if (!protocol_selected_) {
            NoteFailure(MeetingTransportError::ProtocolNotSelected, now_ms);
            return Rejected(MeetingTransportError::ProtocolNotSelected);
        }
        if (queued_messages_ >= max_messages_ || bytes > max_bytes_ - queued_bytes_) {
            NoteFailure(MeetingTransportError::QueueFull, now_ms);
            return Rejected(MeetingTransportError::QueueFull);
        }

        const unsigned long long ticket = next_ticket_++;
        if (queued_messages_ == 0) {
            front_ticket_ = ticket;
        }
        ++queued_messages_;
        queued_bytes_ += bytes;
        return {MeetingMessageDisposition::Queued, MeetingTransportError::None, ticket, true, false};
    }

    bool BeginSend(unsigned long long ticket)
    {
        if (failure_latched_ || in_flight_ticket_ != 0 || queued_messages_ == 0 ||
            ticket != front_ticket_) {
            return false;
        }
        in_flight_ticket_ = ticket;
        return true;
    }

    MeetingEnqueueDisposition_t ResolveSend(
        unsigned long long ticket,
        unsigned long bytes,
        bool has_sequence,
        unsigned int sequence,
        bool send_succeeded,
        unsigned long long now_ms
    ) {
        if (ticket == 0 || ticket != in_flight_ticket_) {
            return Rejected(MeetingTransportError::ServiceStopping);
        }
        in_flight_ticket_ = 0;
        // Resolve the 2000 ms boundary before considering a late success. The
        // message may still have a known local disposition, but the meeting's
        // continuous-pressure failure remains latched.
        CheckDeadline(now_ms);
        if (!send_succeeded) {
            NoteFailure(MeetingTransportError::SendFailed, now_ms);
            CheckDeadline(now_ms);
            return failure_latched_
                ? TerminalResult()
                : MeetingEnqueueDisposition_t{
                      MeetingMessageDisposition::Queued,
                      MeetingTransportError::SendFailed,
                      ticket,
                      true,
                      false,
                  };
        }

        if (queued_messages_ == 0 || bytes > queued_bytes_ || ticket != front_ticket_) {
            NoteFailure(MeetingTransportError::ServiceStopping, now_ms);
            return Rejected(MeetingTransportError::ServiceStopping);
        }
        --queued_messages_;
        queued_bytes_ -= bytes;
        ++front_ticket_;
        last_local_socket_accepted_ticket_ = ticket;
        if (has_sequence) {
            has_local_socket_accepted_sequence_ = true;
            last_local_socket_accepted_sequence_ = sequence;
        }
        if (connected_ && protocol_selected_ && !failure_latched_) {
            ClearTransientFailure();
        }
        return {
            MeetingMessageDisposition::LocalSocketAccepted,
            MeetingTransportError::None,
            ticket,
            false,
            true,
        };
    }

    void NoteFailure(MeetingTransportError error, unsigned long long now_ms)
    {
        if (failure_latched_) {
            return;
        }
        if (!pressure_active_) {
            pressure_active_ = true;
            first_failure_ms_ = now_ms;
            pressure_cause_ = error;
        }
        error_ = error;
        CheckDeadline(now_ms);
    }

    MeetingTransportPolicySnapshot Snapshot(unsigned long long now_ms)
    {
        CheckDeadline(now_ms);
        return {
            connected_,
            protocol_selected_,
            queued_messages_,
            queued_bytes_,
            pressure_active_,
            first_failure_ms_,
            failure_latched_,
            error_,
            pressure_cause_,
            last_local_socket_accepted_ticket_,
            has_local_socket_accepted_sequence_,
            last_local_socket_accepted_sequence_,
        };
    }

    void ResetForNewSession(unsigned long long now_ms)
    {
        if (queued_messages_ != 0 || in_flight_ticket_ != 0) {
            return;
        }
        failure_latched_ = false;
        pressure_active_ = false;
        first_failure_ms_ = 0;
        pressure_cause_ = MeetingTransportError::None;
        error_ = connected_ && protocol_selected_ ? MeetingTransportError::None
                                                   : MeetingTransportError::NotConnected;
        last_local_socket_accepted_ticket_ = 0;
        has_local_socket_accepted_sequence_ = false;
        last_local_socket_accepted_sequence_ = 0;
        if (!connected_) {
            NoteFailure(MeetingTransportError::NotConnected, now_ms);
        } else if (!protocol_selected_) {
            NoteFailure(MeetingTransportError::ProtocolNotSelected, now_ms);
        }
    }

    // A queue may be discarded only after every outstanding entry has the
    // deterministic TerminalFailure disposition. Reconnect alone never calls
    // this; an explicit new meeting session does.
    bool DiscardQueuedAfterTerminal()
    {
        if (!failure_latched_) return false;
        queued_messages_ = 0;
        queued_bytes_ = 0;
        in_flight_ticket_ = 0;
        front_ticket_ = next_ticket_;
        return true;
    }

private:
    void CheckDeadline(unsigned long long now_ms)
    {
        if (!failure_latched_ && pressure_active_ && now_ms - first_failure_ms_ >= pressure_timeout_ms_) {
            failure_latched_ = true;
            error_ = MeetingTransportError::PressureTimeout;
        }
    }

    void ClearTransientFailure()
    {
        pressure_active_ = false;
        first_failure_ms_ = 0;
        pressure_cause_ = MeetingTransportError::None;
        error_ = MeetingTransportError::None;
    }

    static MeetingEnqueueDisposition_t Rejected(MeetingTransportError error)
    {
        return {MeetingMessageDisposition::Rejected, error, 0, false, false};
    }

    MeetingEnqueueDisposition_t TerminalResult() const
    {
        return {
            MeetingMessageDisposition::TerminalFailure,
            error_,
            0,
            false,
            false,
        };
    }

    unsigned long max_messages_;
    unsigned long max_bytes_;
    unsigned long long pressure_timeout_ms_;
    bool connected_ = false;
    bool protocol_selected_ = false;
    unsigned long queued_messages_ = 0;
    unsigned long queued_bytes_ = 0;
    unsigned long long next_ticket_ = 1;
    unsigned long long front_ticket_ = 1;
    unsigned long long in_flight_ticket_ = 0;
    bool pressure_active_ = false;
    unsigned long long first_failure_ms_ = 0;
    bool failure_latched_ = false;
    MeetingTransportError error_ = MeetingTransportError::None;
    MeetingTransportError pressure_cause_ = MeetingTransportError::None;
    unsigned long long last_local_socket_accepted_ticket_ = 0;
    bool has_local_socket_accepted_sequence_ = false;
    unsigned int last_local_socket_accepted_sequence_ = 0;
};

#endif
