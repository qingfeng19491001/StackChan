#include "meeting_send_queue_adapter.h"

struct TestMessage {
    struct Payload {
        unsigned int value = 0;
        unsigned long byte_count = 0;
        unsigned long size() const { return byte_count; }
    } payload;
    bool has_sequence = false;
    unsigned int sequence = 0;
    unsigned long long ticket = 0;
};

struct TestQueue {
    using value_type = TestMessage;
    TestMessage item;
    bool occupied = false;
    bool empty() const { return !occupied; }
    TestMessage& front() { return item; }
    void pop_front() { occupied = false; }
};

struct FakeSender {
    bool succeed = false;
    unsigned int observed_payload = 0;
    bool Send(const TestMessage& message)
    {
        observed_payload = message.payload.value;
        return succeed;
    }
};

extern "C" int run_tests()
{
    MeetingTransportPolicy policy(2, 64, 2000);
    policy.ResetForNewSession(0);
    policy.SetConnection(true, 0);
    policy.SetProtocolSelected(true, 0);
    const auto admitted = policy.Admit(8, true, 7, 0);
    if (!admitted.enqueued) return __LINE__;

    TestQueue queue{{{0xA5, 8}, true, 7, admitted.ticket}, true};
    TestMessage attempt;
    if (!BeginMeetingQueueSend(policy, queue, attempt)) return __LINE__;
    FakeSender sender;
    const bool first_send = sender.Send(attempt);
    const auto failed = FinishMeetingQueueSend(policy, queue, attempt, first_send, 1);
    if (failed.error != MeetingTransportError::SendFailed) return __LINE__;
    if (queue.empty() || queue.front().payload.value != 0xA5 || sender.observed_payload != 0xA5) {
        return __LINE__;
    }

    if (!BeginMeetingQueueSend(policy, queue, attempt)) return __LINE__;
    sender.succeed = true;
    const bool second_send = sender.Send(attempt);
    const auto accepted = FinishMeetingQueueSend(policy, queue, attempt, second_send, 2);
    if (!queue.empty() || !accepted.locally_accepted || accepted.ticket != admitted.ticket) {
        return __LINE__;
    }
    return 0;
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
