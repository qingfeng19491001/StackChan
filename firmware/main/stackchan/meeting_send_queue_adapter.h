#ifndef STACKCHAN_MEETING_SEND_QUEUE_ADAPTER_H
#define STACKCHAN_MEETING_SEND_QUEUE_ADAPTER_H

#include "meeting_transport_policy.h"

// Two-phase production queue seam. Begin is called while the queue lock is
// held, the potentially blocking sender runs without that lock, and Finish is
// called after reacquiring it. Only Finish(success=true) may pop the real front.
template <typename Queue>
bool BeginMeetingQueueSend(
    MeetingTransportPolicy& policy,
    Queue& queue,
    typename Queue::value_type& attempt
) {
    if (queue.empty()) return false;
    attempt = queue.front();
    return policy.BeginSend(attempt.ticket);
}

template <typename Queue>
MeetingEnqueueDisposition_t FinishMeetingQueueSend(
    MeetingTransportPolicy& policy,
    Queue& queue,
    const typename Queue::value_type& attempt,
    bool send_succeeded,
    unsigned long long now_ms
) {
    const auto disposition = policy.ResolveSend(
        attempt.ticket,
        attempt.payload.size(),
        attempt.has_sequence,
        attempt.sequence,
        send_succeeded,
        now_ms
    );
    if (send_succeeded && !queue.empty() && queue.front().ticket == attempt.ticket) {
        queue.pop_front();
    }
    return disposition;
}

#endif
