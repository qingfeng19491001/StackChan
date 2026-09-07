#include "meeting_transport_policy.h"

namespace {

int send_failure_keeps_front_until_success()
{
    MeetingTransportPolicy policy(2, 16, 2000);
    policy.SetConnection(true, 0);
    policy.SetProtocolSelected(true, 0);

    const auto admitted = policy.Admit(8, true, 7, 10);
    if (!admitted.enqueued || admitted.disposition != MeetingMessageDisposition::Queued) {
        return __LINE__;
    }
    if (!policy.BeginSend(admitted.ticket)) {
        return __LINE__;
    }
    policy.ResolveSend(admitted.ticket, 8, true, 7, false, 11);
    if (policy.Snapshot(11).queued_messages != 1) {
        return __LINE__;
    }

    if (!policy.BeginSend(admitted.ticket)) {
        return __LINE__;
    }
    policy.ResolveSend(admitted.ticket, 8, true, 7, true, 12);
    const auto sent = policy.Snapshot(12);
    if (sent.queued_messages != 0 || !sent.has_local_socket_accepted_sequence ||
        sent.last_local_socket_accepted_sequence != 7 ||
        sent.last_local_socket_accepted_ticket != admitted.ticket) {
        return __LINE__;
    }
    return 0;
}

int queue_full_can_recover_before_deadline()
{
    MeetingTransportPolicy policy(2, 16, 2000);
    policy.SetConnection(true, 0);
    policy.SetProtocolSelected(true, 0);
    const auto first = policy.Admit(8, true, 1, 100);
    const auto second = policy.Admit(8, true, 2, 100);
    const auto full = policy.Admit(1, true, 3, 100);
    if (!first.enqueued || !second.enqueued || full.error != MeetingTransportError::QueueFull) {
        return __LINE__;
    }
    if (policy.Snapshot(2099).failure_latched) {
        return __LINE__;
    }
    policy.BeginSend(first.ticket);
    policy.ResolveSend(first.ticket, 8, true, 1, true, 2099);
    if (policy.Snapshot(2100).failure_latched) {
        return __LINE__;
    }
    return 0;
}

int pressure_boundary_is_exact_and_latched_across_reconnect()
{
    MeetingTransportPolicy policy(2, 16, 2000);
    policy.SetConnection(true, 0);
    policy.SetProtocolSelected(true, 0);
    policy.NoteFailure(MeetingTransportError::SendFailed, 500);
    if (policy.Snapshot(2499).failure_latched) {
        return __LINE__;
    }
    const auto timed_out = policy.Snapshot(2500);
    if (!timed_out.failure_latched || timed_out.error != MeetingTransportError::PressureTimeout ||
        timed_out.first_failure_ms != 500) {
        return __LINE__;
    }
    policy.SetConnection(false, 2501);
    policy.SetConnection(true, 2502);
    policy.SetProtocolSelected(true, 2502);
    if (!policy.Snapshot(2502).failure_latched) {
        return __LINE__;
    }
    if (!policy.DiscardQueuedAfterTerminal()) return __LINE__;
    policy.ResetForNewSession(2503);
    if (policy.Snapshot(2503).failure_latched) return __LINE__;
    return 0;
}

int recovery_at_deadline_is_too_late()
{
    MeetingTransportPolicy policy(2, 16, 2000);
    policy.SetConnection(true, 0);
    policy.SetProtocolSelected(true, 0);
    const auto frame = policy.Admit(8, true, 4, 100);
    policy.NoteFailure(MeetingTransportError::QueueFull, 100);
    if (!policy.BeginSend(frame.ticket)) return __LINE__;
    policy.ResolveSend(frame.ticket, 8, true, 4, true, 2100);
    const auto snapshot = policy.Snapshot(2100);
    if (!snapshot.failure_latched || !snapshot.has_local_socket_accepted_sequence ||
        snapshot.last_local_socket_accepted_sequence != 4) return __LINE__;
    return 0;
}

int disconnect_mid_frame_has_known_terminal_disposition()
{
    MeetingTransportPolicy policy(2, 16, 2000);
    policy.SetConnection(true, 0);
    policy.SetProtocolSelected(true, 0);
    const auto frame = policy.Admit(8, true, 9, 10);
    if (!policy.BeginSend(frame.ticket)) {
        return __LINE__;
    }
    policy.SetConnection(false, 11);
    policy.ResolveSend(frame.ticket, 8, true, 9, false, 11);
    if (policy.Snapshot(11).queued_messages != 1 || policy.Snapshot(2010).failure_latched) {
        return __LINE__;
    }
    const auto failed = policy.Snapshot(2011);
    if (!failed.failure_latched || failed.last_local_socket_accepted_ticket == frame.ticket) {
        return __LINE__;
    }
    return 0;
}

}  // namespace

extern "C" int run_tests()
{
    if (const int result = send_failure_keeps_front_until_success()) return result;
    if (const int result = queue_full_can_recover_before_deadline()) return result;
    if (const int result = pressure_boundary_is_exact_and_latched_across_reconnect()) return result;
    if (const int result = recovery_at_deadline_is_too_late()) return result;
    return disconnect_mid_frame_has_known_terminal_disposition();
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
