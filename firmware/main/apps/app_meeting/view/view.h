/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#pragma once

#include <memory>
#include <functional>
#include <string>
#include <smooth_lvgl.hpp>
#include <uitk/short_namespace.hpp>

namespace view {

enum class MeetingUiState {
    Disconnected,
    Ready,
    Preparing,
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
        _start_button->onClick().connect([this]() {
            if (_on_action) {
                _on_action();
            }
        });

        _pairing_hint = std::make_unique<uitk::lvgl_cpp::Label>(*_panel);
        _pairing_hint->setText("SCAN WITH AURO");
        _pairing_hint->setTextFont(&lv_font_montserrat_14);
        _pairing_hint->setTextColor(lv_color_hex(0xB9D9D6));
        _pairing_hint->align(LV_ALIGN_TOP_LEFT, 19, 112);
        _pairing_hint->setHidden(true);
    }

    void setState(MeetingUiState state)
    {
        _state = state;
        switch (state) {
            case MeetingUiState::Disconnected:
                _status->setText("NOT CONNECTED");
                setAction(false, "START FROM AURO");
                break;
            case MeetingUiState::Ready:
                _status->setText("READY");
                setAction(true, "START MEETING");
                break;
            case MeetingUiState::Preparing:
                _status->setText("PREPARING");
                setAction(false, "WAITING FOR AURO");
                break;
            case MeetingUiState::Recording:
                _status->setText("RECORDING");
                setAction(true, "STOP MEETING");
                break;
            case MeetingUiState::Stopping:
                _status->setText("STOPPING");
                setAction(false, "STOPPING...");
                break;
            case MeetingUiState::Error:
                _status->setText("ERROR");
                setAction(false, "TRY AGAIN LATER");
                break;
        }
        applyPairingLayout();
    }

    void setPairingQr(const std::string& pair_uri)
    {
        if (pair_uri.empty()) {
            _pairing_qr.reset();
            _pairing_hint->setHidden(true);
            _has_pairing_qr = false;
            applyPairingLayout();
            return;
        }
        if (!_pairing_qr) {
            _pairing_qr = std::make_unique<uitk::lvgl_cpp::Qrcode>(_panel->get());
            _pairing_qr->setSize(86);
            _pairing_qr->setDarkColor(lv_color_hex(0x082E31));
            _pairing_qr->setLightColor(lv_color_hex(0xE7FFFC));
            _pairing_qr->align(LV_ALIGN_TOP_LEFT, 20, 130);
        }
        _pairing_qr->update(pair_uri);
        _has_pairing_qr = true;
        applyPairingLayout();
    }

    void onAction(std::function<void()> callback)
    {
        _on_action = std::move(callback);
    }

private:
    void setAction(bool enabled, const char* label)
    {
        _start_button->label().setText(label);
        if (enabled) {
            lv_obj_remove_state(_start_button->get(), LV_STATE_DISABLED);
            _start_button->setBgColor(lv_color_hex(0x31C7B5));
            _start_button->label().setTextColor(lv_color_hex(0x082E31));
        } else {
            lv_obj_add_state(_start_button->get(), LV_STATE_DISABLED);
            _start_button->setBgColor(lv_color_hex(0x24575A));
            _start_button->label().setTextColor(lv_color_hex(0x8BAEAF));
        }
    }

    void applyPairingLayout()
    {
        const bool show_pairing = _has_pairing_qr && _state == MeetingUiState::Ready;
        if (_pairing_qr) {
            _pairing_qr->setHidden(!show_pairing);
        }
        _pairing_hint->setHidden(!show_pairing);
        if (show_pairing) {
            _start_button->setSize(168, 48);
            _start_button->align(LV_ALIGN_TOP_RIGHT, -16, 148);
        } else {
            _start_button->setSize(220, 48);
            _start_button->align(LV_ALIGN_TOP_MID, 0, 132);
        }
    }

    std::unique_ptr<uitk::lvgl_cpp::Container> _panel;
    std::unique_ptr<uitk::lvgl_cpp::Label> _title;
    std::unique_ptr<uitk::lvgl_cpp::Label> _status;
    std::unique_ptr<uitk::lvgl_cpp::Button> _start_button;
    std::unique_ptr<uitk::lvgl_cpp::Label> _pairing_hint;
    std::unique_ptr<uitk::lvgl_cpp::Qrcode> _pairing_qr;
    std::function<void()> _on_action;
    MeetingUiState _state = MeetingUiState::Disconnected;
    bool _has_pairing_qr = false;
};

}  // namespace view
