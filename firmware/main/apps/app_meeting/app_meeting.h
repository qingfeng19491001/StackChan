/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once

#include "view/view.h"
#include <memory>
#include <mooncake.h>

class AppMeeting : public mooncake::AppAbility {
public:
    AppMeeting();

    void onCreate() override;
    void onOpen() override;
    void onRunning() override;
    void onClose() override;

private:
    std::unique_ptr<view::MeetingPage> _page;
};
