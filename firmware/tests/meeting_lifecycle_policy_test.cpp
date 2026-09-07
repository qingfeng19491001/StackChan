#include "meeting_callback_gate_state.h"
#include "meeting_stop_policy.h"

namespace {

int disabled_callback_gate_cannot_reach_destroyed_owner()
{
    int owner = 7;
    MeetingCallbackGateState gate(&owner);
    if (gate.Owner() != &owner) {
        return __LINE__;
    }

    gate.Disable();
    if (gate.Owner() != nullptr) {
        return __LINE__;
    }
    return 0;
}

int incomplete_terminal_requires_enqueue_and_flush_disposition()
{
    if (ResolveIncompleteTerminal(true, true) !=
        MeetingIncompleteTerminalDisposition::IncompleteAudioReported) {
        return __LINE__;
    }
    if (ResolveIncompleteTerminal(false, true) !=
        MeetingIncompleteTerminalDisposition::TransportFailed) {
        return __LINE__;
    }
    if (ResolveIncompleteTerminal(true, false) !=
        MeetingIncompleteTerminalDisposition::TransportFailed) {
        return __LINE__;
    }
    if (!ShouldCommitStoppedCommand(true)) return __LINE__;
    if (ShouldCommitStoppedCommand(false)) return __LINE__;
    return 0;
}

}  // namespace

extern "C" int run_tests()
{
    if (const int result = disabled_callback_gate_cannot_reach_destroyed_owner()) {
        return result;
    }
    return incomplete_terminal_requires_enqueue_and_flush_disposition();
}

#ifndef STACKCHAN_WASM_TEST
int main()
{
    return run_tests();
}
#endif
