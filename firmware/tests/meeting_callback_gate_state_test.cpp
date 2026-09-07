#include "meeting_callback_gate_state.h"

extern "C" int run_tests()
{
    int owner = 7;
    MeetingCallbackGateState gate(&owner);

    const auto first = gate.BeginGeneration();
    void* acquired = nullptr;
    if (!gate.TryEnter(first, acquired) || acquired != &owner) return __LINE__;
    if (gate.CanDestroy()) return __LINE__;
    gate.Leave();
    if (!gate.CanDestroy()) return __LINE__;

    const auto second = gate.BeginGeneration();
    if (second == first) return __LINE__;
    acquired = nullptr;
    if (gate.TryEnter(first, acquired)) return __LINE__;
    if (!gate.TryEnter(second, acquired) || acquired != &owner) return __LINE__;
    gate.Leave();

    acquired = nullptr;
    if (!gate.TryEnterCurrent(acquired) || acquired != &owner) return __LINE__;
    gate.Disable();
    if (gate.CanDestroy()) return __LINE__;
    gate.Leave();
    if (!gate.CanDestroy()) return __LINE__;
    if (gate.TryEnter(second, acquired) || gate.TryEnterCurrent(acquired)) return __LINE__;
    return 0;
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
