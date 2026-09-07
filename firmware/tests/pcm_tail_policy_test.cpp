#include "pcm_tail_policy.h"

extern "C" int run_tests()
{
    if (ResolvePcmTail(0) != PcmTailResult::Complete) {
        return __LINE__;
    }
    if (ResolvePcmTail(1) != PcmTailResult::Incomplete) {
        return __LINE__;
    }
    if (ResolvePcmTail(959) != PcmTailResult::Incomplete) {
        return __LINE__;
    }
    return 0;
}

#ifndef STACKCHAN_WASM_TEST
int main()
{
    return run_tests();
}
#endif
