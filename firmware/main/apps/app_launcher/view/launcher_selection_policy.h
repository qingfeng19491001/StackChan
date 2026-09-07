/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once

namespace view {

// Launcher icons are repeated to provide infinite scrolling. Every visual
// attached to an icon (label, background and page dot) must resolve the same
// absolute scroll position back to the same logical app index.
inline int launcher_logical_index(int absolute_index, int app_count)
{
    if (app_count <= 0) {
        return -1;
    }

    int index = absolute_index % app_count;
    return index < 0 ? index + app_count : index;
}

inline int launcher_index_from_scroll(int scroll_value, int item_gap, int app_count)
{
    if (item_gap <= 0 || app_count <= 0) {
        return -1;
    }

    // Round to the nearest icon center. Keep negative values symmetric even
    // though the normal launcher range is positive.
    const int half_gap = item_gap / 2;
    const int absolute_index = scroll_value >= 0
        ? (scroll_value + half_gap) / item_gap
        : (scroll_value - half_gap) / item_gap;
    return launcher_logical_index(absolute_index, app_count);
}

}  // namespace view
