#include "meeting_command_policy.h"

namespace {

int duplicate_and_conflicting_commands_are_deterministic()
{
    const auto lowercase = MeetingCommandKeyFromUuid("00112233-4455-4677-8899-aabbccddeeff");
    const auto uppercase = MeetingCommandKeyFromUuid("00112233-4455-4677-8899-AABBCCDDEEFF");
    const auto different = MeetingCommandKeyFromUuid("00112233-4455-4677-8899-aabbccddee00");
    if (lowercase != uppercase || lowercase == different) return __LINE__;

    MeetingCommandPolicy policy;
    const MeetingCommandKey session1{0, 1};
    const MeetingCommandKey session2{0, 2};
    const MeetingCommandKey command11{0, 11};
    const MeetingCommandKey command12{0, 12};
    const MeetingCommandKey command21{0, 21};
    const MeetingCommandKey command22{0, 22};
    if (policy.OnStart(session1, command11) != MeetingCommandDecision::Accept) return __LINE__;
    if (policy.OnStart(session1, command11) != MeetingCommandDecision::Duplicate) return __LINE__;
    if (policy.OnStart(session2, command11) != MeetingCommandDecision::Conflict) return __LINE__;
    if (policy.OnStart(session1, command12) != MeetingCommandDecision::Conflict) return __LINE__;
    if (policy.OnStop(session2, command21) != MeetingCommandDecision::Conflict) return __LINE__;
    if (policy.OnStop(session1, command21) != MeetingCommandDecision::Accept) return __LINE__;
    policy.MarkStopped(session1, command21);
    if (policy.OnStop(session1, command21) != MeetingCommandDecision::Duplicate) return __LINE__;
    if (policy.OnStop(session1, command22) != MeetingCommandDecision::Conflict) return __LINE__;
    if (policy.OnStart(session2, command12) != MeetingCommandDecision::Accept) return __LINE__;
    if (policy.OnStop(session2, command22) != MeetingCommandDecision::Accept) return __LINE__;
    policy.MarkStopped(session2, command22);
    if (policy.OnStart(session1, command11) != MeetingCommandDecision::Duplicate) return __LINE__;
    if (IsCorrelatedMeetingError(
            true, session2, command12, true, command22,
            true, session1, true, command11)) return __LINE__;
    if (!IsCorrelatedMeetingError(
            true, session2, command12, true, command22,
            true, session2, true, command12)) return __LINE__;
    if (!IsCorrelatedMeetingError(
            true, session2, command12, true, command22,
            true, session2, true, command22)) return __LINE__;
    if (IsCorrelatedMeetingError(
            true, session2, command12, true, command22,
            false, session2, true, command22)) return __LINE__;
    MeetingCommandPolicy failed;
    if (failed.OnStart(session1, command11) != MeetingCommandDecision::Accept) return __LINE__;
    failed.MarkStartFailed(session1, command11);
    if (failed.OnStart(session1, command11) != MeetingCommandDecision::Duplicate) return __LINE__;
    if (failed.OnStart(session1, command12) != MeetingCommandDecision::Conflict) return __LINE__;
    if (failed.OnStop(session1, command21) != MeetingCommandDecision::Conflict) return __LINE__;
    if (failed.OnStart(session2, command12) != MeetingCommandDecision::Accept) return __LINE__;
    return 0;
}

}  // namespace

extern "C" int run_tests() { return duplicate_and_conflicting_commands_are_deterministic(); }

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
