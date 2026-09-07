/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "hal.h"

#include "utils/secret_logic/secret_logic.h"

#include <ArduinoJson.hpp>
#include <board.h>

namespace {

constexpr size_t kMaxPairingResponseBytes = 4096;

MeetingPairingStatus statusFromHttpCode(int status_code)
{
    switch (status_code) {
        case 401:
            return MeetingPairingStatus::Unauthorized;
        case 409:
            return MeetingPairingStatus::DeviceOffline;
        case 400:
            return MeetingPairingStatus::ProtocolUnsupported;
        default:
            return MeetingPairingStatus::NetworkUnavailable;
    }
}

}  // namespace

MeetingPairingNonce_t Hal::requestMeetingPairingNonce()
{
    MeetingPairingNonce_t result;
    const auto credential = secret_logic::get_device_credential();
    if (credential.empty()) {
        result.status = MeetingPairingStatus::MissingCredential;
        return result;
    }

    auto network = Board::GetInstance().GetNetwork();
    if (!network) {
        result.status = MeetingPairingStatus::NetworkUnavailable;
        return result;
    }
    auto http = network->CreateHttp(0);
    if (!http) {
        result.status = MeetingPairingStatus::NetworkUnavailable;
        return result;
    }

    ArduinoJson::JsonDocument request;
    request["mac"] = getFactoryMacString();
    request["protocolVersion"] = 1;
    std::string payload;
    ArduinoJson::serializeJson(request, payload);

    http->SetHeader("Authorization", "Device " + credential);
    http->SetHeader("Content-Type", "application/json");
    http->SetContent(std::move(payload));
    const auto url = secret_logic::get_server_url() + "/stackChan/pairing-nonce";
    if (!http->Open("POST", url)) {
        result.status = MeetingPairingStatus::NetworkUnavailable;
        return result;
    }

    const int status_code = http->GetStatusCode();
    if (status_code != 200) {
        result.status = statusFromHttpCode(status_code);
        http->Close();
        return result;
    }

    std::string response = http->ReadAll();
    http->Close();
    if (response.empty() || response.size() > kMaxPairingResponseBytes) {
        result.status = MeetingPairingStatus::InvalidResponse;
        return result;
    }

    ArduinoJson::JsonDocument document;
    const auto parse_error = ArduinoJson::deserializeJson(document, response);
    if (parse_error || !document["pairUri"].is<const char*>() ||
        !document["expiresAt"].is<int64_t>()) {
        result.status = MeetingPairingStatus::InvalidResponse;
        return result;
    }

    result.pairUri = document["pairUri"].as<std::string>();
    result.expiresAt = document["expiresAt"].as<int64_t>();
    if (result.pairUri.rfind("stackchan://pair?", 0) != 0 || result.expiresAt <= 0) {
        result.pairUri.clear();
        result.expiresAt = 0;
        result.status = MeetingPairingStatus::InvalidResponse;
        return result;
    }
    result.status = MeetingPairingStatus::Ready;
    return result;
}
