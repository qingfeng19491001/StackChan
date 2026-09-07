#include "audio_lifecycle_policy.h"
#include "audio_output_dispatch_policy.h"

namespace {

int two_detached_frames_block_every_inter_callback_gap()
{
    AudioLifecyclePolicy lifecycle;
    AudioOutputDispatchPolicy dispatch;
    lifecycle.OpenIntake();
    lifecycle.CloseIntake();
    dispatch.DetachFrames(2);

    if (CanDeclareAudioDrain(
            lifecycle.ProducersQuiescent(true), true, dispatch.IsQuiescent()
        )) {
        return __LINE__;
    }

    if (!dispatch.BeginCallback() || dispatch.IsQuiescent()) {
        return __LINE__;
    }
    dispatch.EndCallback();

    // The first callback and its encode can be fully drained here. The second
    // detached frame must still keep the overall barrier closed.
    if (CanDeclareAudioDrain(
            lifecycle.ProducersQuiescent(true), true, dispatch.IsQuiescent()
        )) {
        return __LINE__;
    }

    if (!dispatch.BeginCallback() || dispatch.IsQuiescent()) {
        return __LINE__;
    }
    dispatch.EndCallback();

    if (!CanDeclareAudioDrain(
            lifecycle.ProducersQuiescent(true), true, dispatch.IsQuiescent()
        )) {
        return __LINE__;
    }

    // Once drain can succeed, no detached frame remains that can begin a later
    // callback and enqueue a post-drain packet.
    if (dispatch.BeginCallback()) {
        return __LINE__;
    }
    return 0;
}

int active_callback_blocks_drain_until_dispatch_returns()
{
    AudioLifecyclePolicy lifecycle;
    AudioOutputDispatchPolicy dispatch;
    lifecycle.CloseIntake();
    dispatch.DetachFrames(1);
    if (!dispatch.BeginCallback()) {
        return __LINE__;
    }
    if (CanDeclareAudioDrain(true, true, dispatch.IsQuiescent())) {
        return __LINE__;
    }
    dispatch.EndCallback();
    if (!CanDeclareAudioDrain(true, true, dispatch.IsQuiescent())) {
        return __LINE__;
    }
    return 0;
}

}  // namespace

extern "C" int run_tests()
{
    if (const int result = two_detached_frames_block_every_inter_callback_gap()) {
        return result;
    }
    return active_callback_blocks_drain_until_dispatch_returns();
}

#ifndef STACKCHAN_WASM_TEST
int main()
{
    return run_tests();
}
#endif
