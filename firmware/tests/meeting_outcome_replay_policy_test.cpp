#include "meeting_outcome_replay_policy.h"

extern "C" int run_tests()
{
    if (ResolveMeetingOutcomeReplay(false, 0, false, 0, false) !=
        MeetingOutcomeReplayAction::Missing) return __LINE__;
    if (ResolveMeetingOutcomeReplay(true, 7, true, 6, false) !=
        MeetingOutcomeReplayAction::KeepPending) return __LINE__;
    if (ResolveMeetingOutcomeReplay(true, 7, true, 7, false) !=
        MeetingOutcomeReplayAction::ReplayExactBytes) return __LINE__;
    if (ResolveMeetingOutcomeReplay(true, 7, false, 7, false) !=
        MeetingOutcomeReplayAction::ReplayExactBytes) return __LINE__;
    if (ResolveMeetingOutcomeReplay(true, 7, true, 6, true) !=
        MeetingOutcomeReplayAction::TerminalFailure) return __LINE__;
    return 0;
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
