#include "audio_runtime_state.h"

namespace {

int startup_rollback_stops_timer_and_allows_retry()
{
    AudioRuntimeState state;
    if (!state.BeginStart()) {
        return __LINE__;
    }
    state.MarkTimerStarted();
    if (!state.TimerRunning()) {
        return __LINE__;
    }

    state.RollbackStart();
    if (state.TimerRunning() || state.StartActive()) {
        return __LINE__;
    }
    if (!state.BeginStart()) {
        return __LINE__;
    }
    return 0;
}

int encode_failure_never_resolves_to_complete()
{
    AudioRuntimeState state;
    state.BeginStart();
    state.MarkEncodeFailed();

    if (state.ResolveDrain(false) != AudioRuntimeDrainDisposition::EncodeFailed) {
        return __LINE__;
    }
    if (state.ResolveDrain(true) != AudioRuntimeDrainDisposition::EncodeFailed) {
        return __LINE__;
    }
    return 0;
}

int stop_and_restart_clear_capture_metadata()
{
    AudioRuntimeState state;
    state.BeginStart();
    state.MarkCaptureMetadataPending();
    state.MarkEncodeFailed();
    if (!state.CaptureMetadataPending()) {
        return __LINE__;
    }

    state.ResetAfterStop();
    if (state.CaptureMetadataPending() || state.EncodeFailed()) {
        return __LINE__;
    }
    if (!state.BeginStart() || state.CaptureMetadataPending()) {
        return __LINE__;
    }
    return 0;
}

int healthy_and_partial_processor_drains_are_distinct()
{
    AudioRuntimeState state;
    state.BeginStart();
    if (state.ResolveDrain(false) != AudioRuntimeDrainDisposition::Complete) {
        return __LINE__;
    }
    if (state.ResolveDrain(true) != AudioRuntimeDrainDisposition::IncompleteProcessorTail) {
        return __LINE__;
    }
    return 0;
}

int readiness_requires_processor_preparation()
{
    AudioRuntimeState state;
    if (state.ProcessorPrepared()) {
        return __LINE__;
    }
    state.MarkProcessorPrepared();
    if (!state.ProcessorPrepared()) {
        return __LINE__;
    }
    state.MarkProcessorReleased();
    if (state.ProcessorPrepared()) {
        return __LINE__;
    }
    return 0;
}

}  // namespace

extern "C" int run_tests()
{
    if (const int result = startup_rollback_stops_timer_and_allows_retry()) {
        return result;
    }
    if (const int result = encode_failure_never_resolves_to_complete()) {
        return result;
    }
    if (const int result = stop_and_restart_clear_capture_metadata()) {
        return result;
    }
    if (const int result = healthy_and_partial_processor_drains_are_distinct()) {
        return result;
    }
    return readiness_requires_processor_preparation();
}

#ifndef STACKCHAN_WASM_TEST
int main()
{
    return run_tests();
}
#endif
