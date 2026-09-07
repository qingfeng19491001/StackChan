#include "meeting_service_lifecycle_policy.h"

extern "C" int run_tests()
{
    MeetingServiceLifecyclePolicy policy;
    if (!policy.BeginEnsure() || policy.AcquireReady()) return __LINE__;
    if (policy.BeginEnsure()) return __LINE__;
    policy.PublishReady();
    if (!policy.AcquireReady()) return __LINE__;
    policy.BeginStop();
    if (policy.AcquireReady()) return __LINE__;
    policy.PublishStopped();
    if (!policy.BeginEnsure()) return __LINE__;
    return 0;
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
