#include <cassert>
#include <cstdint>
#include <vector>

#include "pcm_frame_accumulator.h"

namespace {

void complete_frames_are_emitted_without_a_tail()
{
    PcmFrameAccumulator accumulator(4);
    std::vector<std::vector<int16_t>> frames;

    accumulator.Append({1, 2, 3}, [&](std::vector<int16_t>&& frame) {
        frames.push_back(std::move(frame));
    });
    accumulator.Append({4, 5, 6, 7, 8}, [&](std::vector<int16_t>&& frame) {
        frames.push_back(std::move(frame));
    });

    assert(frames.size() == 2);
    assert((frames[0] == std::vector<int16_t>{1, 2, 3, 4}));
    assert((frames[1] == std::vector<int16_t>{5, 6, 7, 8}));
    assert(accumulator.Flush() == PcmTailResult::Complete);
}

void partial_tail_is_reported_and_never_claimed_complete()
{
    PcmFrameAccumulator accumulator(4);
    accumulator.Append({1, 2, 3}, [](std::vector<int16_t>&&) {});

    assert(accumulator.Flush() == PcmTailResult::Incomplete);
    assert(accumulator.empty());
}

void reset_discards_only_after_returning_an_incomplete_result()
{
    PcmFrameAccumulator accumulator(4);
    accumulator.Append({1}, [](std::vector<int16_t>&&) {});

    const auto result = accumulator.Flush();
    assert(result == PcmTailResult::Incomplete);
    assert(accumulator.Flush() == PcmTailResult::Complete);
}

}  // namespace

int main()
{
    complete_frames_are_emitted_without_a_tail();
    partial_tail_is_reported_and_never_claimed_complete();
    reset_discards_only_after_returning_an_incomplete_result();
    return 0;
}
