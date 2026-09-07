#include "audio_lifecycle_policy.h"
#include "audio_worker_startup_policy.h"

namespace {

int dequeued_encoder_work_blocks_drain_success()
{
    AudioLifecyclePolicy policy;
    policy.OpenIntake();
    if (!policy.TryBeginEncoderWork()) {
        return __LINE__;
    }
    policy.CloseIntake();
    if (policy.ProducersQuiescent(true)) {
        return __LINE__;
    }
    policy.EndEncoderWork();
    if (!policy.ProducersQuiescent(true)) {
        return __LINE__;
    }
    return 0;
}

int partial_creation_and_join_timeout_preserve_liveness()
{
    AudioLifecyclePolicy policy;
    policy.ResetWorkers();
    policy.MarkWorkerCreated(AudioWorker::Input);
    policy.MarkWorkerCreated(AudioWorker::Codec);
    policy.MarkWorkerExited(AudioWorker::Input);
    if (policy.CanDestroy()) {
        return __LINE__;
    }
    policy.MarkWorkerExited(AudioWorker::Codec);
    if (!policy.CanDestroy()) {
        return __LINE__;
    }

    policy.ResetWorkers();
    policy.MarkWorkerCreated(AudioWorker::Input);
    policy.MarkWorkerExited(AudioWorker::Input);
    if (!policy.AllWorkersExited()) {
        return __LINE__;
    }
    return 0;
}

int one_hundred_lifecycle_cycles_return_destroyable()
{
    AudioLifecyclePolicy policy;
    for (int cycle = 0; cycle < 100; ++cycle) {
        policy.ResetWorkers();
        policy.MarkWorkerCreated(AudioWorker::Input);
        policy.MarkWorkerCreated(AudioWorker::Output);
        policy.MarkWorkerCreated(AudioWorker::Codec);
        policy.OpenIntake();
        if (!policy.TryBeginInputWork()) {
            return __LINE__;
        }
        policy.EndInputWork();
        policy.CloseIntake();
        policy.MarkWorkerExited(AudioWorker::Input);
        policy.MarkWorkerExited(AudioWorker::Output);
        policy.MarkWorkerExited(AudioWorker::Codec);
        if (!policy.CanDestroy()) {
            return __LINE__;
        }
    }
    return 0;
}

struct FakeWorkerRuntime {
    bool create_results[3] = {true, true, true};
    bool rollback_result = true;
    int create_calls = 0;
    int live_workers = 0;
    int joined_workers = 0;
    int rollback_calls = 0;

    bool Create(int index)
    {
        ++create_calls;
        if (create_results[index]) {
            ++live_workers;
            return true;
        }
        return false;
    }

    bool RollbackAndJoin()
    {
        ++rollback_calls;
        if (!rollback_result) {
            return false;
        }
        joined_workers += live_workers;
        live_workers = 0;
        return true;
    }
};

int production_startup_seam_rolls_back_only_created_workers()
{
    FakeWorkerRuntime runtime;
    runtime.create_results[1] = false;

    const auto result = StartAudioWorkers(
        [&runtime]() { return runtime.Create(0); },
        [&runtime]() { return runtime.Create(1); },
        [&runtime]() { return runtime.Create(2); },
        [&runtime]() { return runtime.RollbackAndJoin(); }
    );
    if (result != AudioWorkerStartupResult::CreationFailed ||
        runtime.create_calls != 3 || runtime.rollback_calls != 1 ||
        runtime.joined_workers != 2 || runtime.live_workers != 0) {
        return __LINE__;
    }
    return 0;
}

int production_startup_seam_surfaces_rollback_timeout()
{
    FakeWorkerRuntime runtime;
    runtime.create_results[2] = false;
    runtime.rollback_result = false;

    const auto result = StartAudioWorkers(
        [&runtime]() { return runtime.Create(0); },
        [&runtime]() { return runtime.Create(1); },
        [&runtime]() { return runtime.Create(2); },
        [&runtime]() { return runtime.RollbackAndJoin(); }
    );
    if (result != AudioWorkerStartupResult::RollbackFailed ||
        runtime.rollback_calls != 1 || runtime.joined_workers != 0 ||
        runtime.live_workers != 2) {
        return __LINE__;
    }
    return 0;
}

}  // namespace

extern "C" int run_tests()
{
    if (const int result = dequeued_encoder_work_blocks_drain_success()) {
        return result;
    }
    if (const int result = partial_creation_and_join_timeout_preserve_liveness()) {
        return result;
    }
    if (const int result = one_hundred_lifecycle_cycles_return_destroyable()) {
        return result;
    }
    if (const int result = production_startup_seam_rolls_back_only_created_workers()) {
        return result;
    }
    return production_startup_seam_surfaces_rollback_timeout();
}

#ifndef STACKCHAN_WASM_TEST
int main()
{
    return run_tests();
}
#endif
