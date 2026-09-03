/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "app_meeting.h"

#include <apps/common/common.h>
#include <assets/assets.h>
#include <hal/hal.h>
#include <mooncake_log.h>

using namespace mooncake;

AppMeeting::AppMeeting()
{
    setAppInfo().name = "MEETING";

    // Phase 0 reuses an existing bundled icon. A dedicated asset can replace it
    // without changing the launcher registration or Meeting UI lifecycle.
    static auto icon  = assets::get_image("icon_ezdata.bin");
    setAppInfo().icon = (void*)&icon;

    static uint32_t theme_color = 0x31C7B5;
    setAppInfo().userData       = (void*)&theme_color;
}

void AppMeeting::onCreate()
{
    mclog::tagInfo(getAppInfo().name, "on create");
}

void AppMeeting::onOpen()
{
    mclog::tagInfo(getAppInfo().name, "on open");

    LvglLockGuard lock;
    _page = std::make_unique<view::MeetingPage>();
    _page->setState(view::MeetingUiState::Disconnected);

    view::create_home_indicator([this]() { close(); }, 0x75E4D8, 0x082E31);
    view::create_status_bar(0x75E4D8, 0x082E31);
}

void AppMeeting::onRunning()
{
    LvglLockGuard lock;
    view::update_home_indicator();
    view::update_status_bar();
}

void AppMeeting::onClose()
{
    mclog::tagInfo(getAppInfo().name, "on close");

    LvglLockGuard lock;
    _page.reset();
    view::destroy_home_indicator();
    view::destroy_status_bar();
}
