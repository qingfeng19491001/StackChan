#include "launcher_selection_policy.h"

namespace {

int repeated_launcher_visuals_share_one_logical_index()
{
    constexpr int app_count = 8;
    constexpr int gap = 320;

    for (int copy = 0; copy < 5; ++copy) {
        for (int app = 0; app < app_count; ++app) {
            const int scroll = (copy * app_count + app) * gap;
            const int label_index = view::launcher_index_from_scroll(scroll, gap, app_count);
            const int background_index = view::launcher_index_from_scroll(scroll, gap, app_count);
            const int indicator_index = view::launcher_index_from_scroll(scroll, gap, app_count);
            if (label_index != app || background_index != app || indicator_index != app) {
                return __LINE__;
            }
        }
    }

    if (view::launcher_index_from_scroll(2 * app_count * gap + gap / 2 - 1, gap, app_count) != 0) {
        return __LINE__;
    }
    if (view::launcher_index_from_scroll(2 * app_count * gap + gap / 2, gap, app_count) != 1) {
        return __LINE__;
    }
    if (view::launcher_logical_index(-1, app_count) != app_count - 1) return __LINE__;
    if (view::launcher_index_from_scroll(0, 0, app_count) != -1) return __LINE__;
    if (view::launcher_index_from_scroll(0, gap, 0) != -1) return __LINE__;
    return 0;
}

}  // namespace

extern "C" int run_tests() { return repeated_launcher_visuals_share_one_logical_index(); }

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
