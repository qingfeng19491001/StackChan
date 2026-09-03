/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once

#include <memory>
#include <smooth_lvgl.hpp>
#include <uitk/short_namespace.hpp>

namespace view {

enum class MeetingUiState {
    Disconnected,
    Ready,
    Recording,
    Stopping,
    Error,
};

class MeetingPage {
public:
    MeetingPage()
    {
        _panel = std::make_unique<uitk::lvgl_cpp::Container>(lv_screen_active());
        _panel->setSize(320, 240);
        _panel->setAlign(LV_ALIGN_CENTER);
        _panel->setBgColor(lv_color_hex(0x082E31));
        _panel->setPadding(0, 0, 0, 0);
        _panel->setRadius(0);
        _panel->setBorderWidth(0);

        _title = std::make_unique<uitk::lvgl_cpp::Label>(*_panel);
        _title->setText("MEETING MINUTES");
        _title->setTextFont(&lv_font_montserrat_24);
        _title->setTextColor(lv_color_hex(0xE7FFFC));
        _title->align(LV_ALIGN_TOP_MID, 0, 44);

        _status = std::make_unique<uitk::lvgl_cpp::Label>(*_panel);
        _status->setTextFont(&lv_font_montserrat_20);
        _status->setTextColor(lv_color_hex(0x75E4D8));
        _status->align(LV_ALIGN_TOP_MID, 0, 91);

        _start_button = std::make_unique<uitk::lvgl_cpp::Button>(*_panel);
        _start_button->setSize(220, 48);
        _start_button->align(LV_ALIGN_TOP_MID, 0, 132);
        _start_button->setRadius(18);
        _start_button->setBgColor(lv_color_hex(0x24575A));
        _start_button->setBorderWidth(0);
        _start_button->setShadowWidth(0);
        _start_button->label().setText("START FROM AURO");
        _start_button->label().setTextFont(&lv_font_montserrat_16);
        _start_button->label().setTextColor(lv_color_hex(0x8BAEAF));
        lv_obj_add_state(_start_button->get(), LV_STATE_DISABLED);
    }

    void setState(MeetingUiState state)
    {
        switch (state) {
            case MeetingUiState::Disconnected:
                _status->setText("NOT CONNECTED");
                break;
            case MeetingUiState::Ready:
                _status->setText("READY");
                break;
            case MeetingUiState::Recording:
                _status->setText("RECORDING");
                break;
            case MeetingUiState::Stopping:
                _status->setText("STOPPING");
                break;
            case MeetingUiState::Error:
                _status->setText("ERROR");
                break;
        }
    }

private:
    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::unique_ptr<uitk::lvgl_cpp::Label> _title;
    std::unique_ptr<uitk::lvgl_cpp::Label> _status;
    std::unique_ptr<uitk::lvgl_cpp::Button> _start_button;
};

}  // namespace view
