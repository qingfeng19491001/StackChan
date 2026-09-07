#include <cassert>
#include <cstdint>

#include "audio_lifecycle_state.h"

namespace {

void encoder_dequeued_item_prevents_false_quiescence()
{
    AudioLifecycleState state;
    state.OpenIntake();
    assert(state.TryBeginEncoderWork());
    state.CloseIntake();

    assert(!state.ProducersQuiescent(true));

    state.EndEncoderWork();
    assert(state.ProducersQuiescent(true));
}

void closed_intake_rejects_late_input()
{
    AudioLifecycleState state;
    state.OpenIntake();
    state.CloseIntake();

    assert(!state.TryBeginInputWork());
    assert(state.InputQuiescent());
}

void join_timeout_never_makes_live_workers_destroyable()
{
    AudioLifecycleState state;
    state.ResetWorkers();
    state.MarkWorkerCreated(AudioWorker::Input);
    state.MarkWorkerCreated(AudioWorker::Codec);
    state.MarkWorkerExited(AudioWorker::Input);

    assert(!state.AllWorkersExited());
    assert(!state.CanDestroy());

    state.MarkWorkerExited(AudioWorker::Codec);
    assert(state.AllWorkersExited());
    assert(state.CanDestroy());
}

void partial_task_creation_can_join_only_the_created_workers()
{
    AudioLifecycleState state;
    state.ResetWorkers();
    state.MarkWorkerCreated(AudioWorker::Input);
    state.MarkWorkerExited(AudioWorker::Input);

    assert(state.AllWorkersExited());
    assert(state.CanDestroy());
}

void repeated_lifecycle_returns_to_destroyable_state()
{
    AudioLifecycleState state;
    for (int cycle = 0; cycle < 100; ++cycle) {
        state.ResetWorkers();
        state.MarkWorkerCreated(AudioWorker::Input);
        state.MarkWorkerCreated(AudioWorker::Output);
        state.MarkWorkerCreated(AudioWorker::Codec);
        state.OpenIntake();
        assert(state.TryBeginInputWork());
        state.EndInputWork();
        state.CloseIntake();
        state.MarkWorkerExited(AudioWorker::Input);
        state.MarkWorkerExited(AudioWorker::Output);
        state.MarkWorkerExited(AudioWorker::Codec);
        assert(state.InputQuiescent());
        assert(state.AllWorkersExited());
        assert(state.CanDestroy());
    }
}

}  // namespace

int main()
{
    encoder_dequeued_item_prevents_false_quiescence();
    closed_intake_rejects_late_input();
    join_timeout_never_makes_live_workers_destroyable();
    partial_task_creation_can_join_only_the_created_workers();
    repeated_lifecycle_returns_to_destroyable_state();
    return 0;
}
