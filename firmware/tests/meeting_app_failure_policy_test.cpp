#include "meeting_app_failure_policy.h"
#include "meeting_transport_policy.h"

extern "C" int run_tests()
{
    if (ResolveMeetingAppFailure(false, false) != MeetingAppFailureAction::Continue) {
        return __LINE__;
    }
    if (ResolveMeetingAppFailure(true, false) != MeetingAppFailureAction::AbortToError) {
        return __LINE__;
    }
    if (ResolveMeetingAppFailure(false, true) != MeetingAppFailureAction::AbortToError) {
        return __LINE__;
    }
    MeetingTransportPolicy transport(4, 64, 2000);
    transport.SetConnection(true, 100);
    transport.SetProtocolSelected(true, 100);
    transport.ResetForNewSession(100);
    MeetingAppFailureState app;
    transport.SetConnection(false, 100);
    auto at_1999 = transport.Snapshot(2099);
    if (at_1999.failure_latched) return __LINE__;
    if (app.Observe(false, at_1999.failure_latched) != MeetingAppFailureAction::Continue) {
        return __LINE__;
    }
    if (app.ErrorLatched()) return __LINE__;
    auto at_2000 = transport.Snapshot(2100);
    if (app.Observe(false, at_2000.failure_latched) != MeetingAppFailureAction::AbortToError ||
        !app.ErrorLatched()) return __LINE__;
    transport.SetConnection(true, 2101);
    transport.SetProtocolSelected(true, 2101);
    if (!transport.Snapshot(2101).failure_latched || !app.ErrorLatched()) return __LINE__;
    app.OnAcceptedStart();
    if (app.ErrorLatched()) return __LINE__;
    return 0;
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
