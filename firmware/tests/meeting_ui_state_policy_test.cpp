#include "meeting_ui_state_policy.h"

namespace {

int idle_state_requires_selected_protocol_for_ready()
{
    if (ResolveMeetingUiState(true, true, false, false, false) !=
        MeetingUiStateResolution::Ready) return __LINE__;
    if (ResolveMeetingUiState(true, false, false, false, false) !=
        MeetingUiStateResolution::Preparing) return __LINE__;
    if (ResolveMeetingUiState(false, false, false, false, false) !=
        MeetingUiStateResolution::Disconnected) return __LINE__;
    return 0;
}

int pending_local_stop_recovers_after_deadline()
{
    if (ResolveMeetingUiState(true, true, true, false, true) !=
        MeetingUiStateResolution::Stopping) return __LINE__;
    if (ResolveMeetingUiState(true, true, true, false, false) !=
        MeetingUiStateResolution::Recording) return __LINE__;
    if (ResolveMeetingUiState(false, false, true, false, false) !=
        MeetingUiStateResolution::Reconnecting) return __LINE__;
    return 0;
}

int active_recording_reports_reconnecting_when_transport_is_lost()
{
    if (ResolveMeetingUiState(false, false, true, false, false) !=
        MeetingUiStateResolution::Reconnecting) return __LINE__;
    return 0;
}

}  // namespace

extern "C" int run_tests()
{
    if (const int result = idle_state_requires_selected_protocol_for_ready()) return result;
    if (const int result = pending_local_stop_recovers_after_deadline()) return result;
    return active_recording_reports_reconnecting_when_transport_is_lost();
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
