#ifndef STACKCHAN_MEETING_CONTROL_POLICY_H
#define STACKCHAN_MEETING_CONTROL_POLICY_H

enum class MeetingInboundAction {
    None = 0,
    ProtocolSelected,
    Start,
    Stop,
    Error,
};

enum class MeetingControlError {
    None = 0,
    MalformedJson,
    PayloadTooLarge,
    InvalidType,
    UnsupportedVersion,
    UnknownAction,
    WrongDirection,
    InvalidUuid,
    UnsupportedCapability,
    UnsupportedAudio,
    InvalidField,
};

struct MeetingControlDocument {
    unsigned long payload_bytes = 0;
    unsigned int root_field_count = 0;
    bool root_is_object = true;
    bool protocol_version_is_integer = false;
    int protocol_version = 0;
    bool action_is_string = false;
    const char* action = nullptr;
    bool message_id_is_string = false;
    const char* message_id = nullptr;
    bool command_id_is_string = false;
    const char* command_id = nullptr;
    bool session_id_is_string = false;
    const char* session_id = nullptr;
    bool capabilities_is_array = false;
    unsigned int capability_count = 0;
    unsigned int meeting_v1_capability_count = 0;
    bool audio_is_object = false;
    unsigned int audio_field_count = 0;
    bool codec_is_string = false;
    const char* codec = nullptr;
    bool sample_rate_is_integer = false;
    int sample_rate = 0;
    bool channels_is_integer = false;
    int channels = 0;
    bool frame_duration_is_integer = false;
    int frame_duration_ms = 0;
    bool code_is_string = false;
    const char* code = nullptr;
    bool mac_present = false;
};

struct MeetingControlValidation {
    MeetingInboundAction action = MeetingInboundAction::None;
    MeetingControlError error = MeetingControlError::None;
};

constexpr bool MeetingStringEquals(const char* left, const char* right)
{
    if (left == nullptr || right == nullptr) return false;
    while (*left != '\0' && *right != '\0') {
        if (*left++ != *right++) return false;
    }
    return *left == *right;
}

constexpr bool IsMeetingUuid(const char* value)
{
    if (value == nullptr) return false;
    for (unsigned int index = 0; index < 36; ++index) {
        const char character = value[index];
        if (character == '\0') return false;
        if (index == 8 || index == 13 || index == 18 || index == 23) {
            if (character != '-') return false;
            continue;
        }
        const bool digit = character >= '0' && character <= '9';
        const bool lower = character >= 'a' && character <= 'f';
        const bool upper = character >= 'A' && character <= 'F';
        if (!digit && !lower && !upper) return false;
    }
    return value[36] == '\0';
}

constexpr bool IsStableMeetingErrorCode(const char* code)
{
    return MeetingStringEquals(code, "UNAUTHORIZED") ||
        MeetingStringEquals(code, "NOT_BOUND") ||
        MeetingStringEquals(code, "DEVICE_OFFLINE") ||
        MeetingStringEquals(code, "SESSION_BUSY") ||
        MeetingStringEquals(code, "START_TIMEOUT") ||
        MeetingStringEquals(code, "AUDIO_GAP") ||
        MeetingStringEquals(code, "SERVER_DISCONNECTED") ||
        MeetingStringEquals(code, "DEVICE_ERROR") ||
        MeetingStringEquals(code, "WRITE_FAILED") ||
        MeetingStringEquals(code, "PROTOCOL_UNSUPPORTED");
}

constexpr MeetingControlValidation ValidateInboundMeetingControl(
    const MeetingControlDocument& doc
) {
    if (doc.payload_bytes == 0 || doc.payload_bytes > 16UL * 1024UL) {
        return {MeetingInboundAction::None, MeetingControlError::PayloadTooLarge};
    }
    if (!doc.root_is_object || !doc.protocol_version_is_integer || !doc.action_is_string ||
        !doc.message_id_is_string) {
        return {MeetingInboundAction::None, MeetingControlError::InvalidType};
    }
    if (doc.protocol_version != 1) {
        return {MeetingInboundAction::None, MeetingControlError::UnsupportedVersion};
    }
    if (!IsMeetingUuid(doc.message_id)) {
        return {MeetingInboundAction::None, MeetingControlError::InvalidUuid};
    }

    if (MeetingStringEquals(doc.action, "protocol.selected")) {
        if (doc.root_field_count != 4) {
            return {MeetingInboundAction::ProtocolSelected, MeetingControlError::InvalidField};
        }
        if (!doc.capabilities_is_array) {
            return {MeetingInboundAction::ProtocolSelected, MeetingControlError::InvalidType};
        }
        if (doc.capability_count != 1 || doc.meeting_v1_capability_count != 1) {
            return {MeetingInboundAction::ProtocolSelected, MeetingControlError::UnsupportedCapability};
        }
        return {MeetingInboundAction::ProtocolSelected, MeetingControlError::None};
    }

    if (MeetingStringEquals(doc.action, "meeting.start-requested") ||
        MeetingStringEquals(doc.action, "meeting.stop-requested") ||
        MeetingStringEquals(doc.action, "meeting.start-offered") ||
        MeetingStringEquals(doc.action, "meeting.accept") ||
        MeetingStringEquals(doc.action, "meeting.reattach")) {
        return {MeetingInboundAction::None, MeetingControlError::WrongDirection};
    }

    const bool is_start = MeetingStringEquals(doc.action, "meeting.start");
    const bool is_stop = MeetingStringEquals(doc.action, "meeting.stop");
    if (is_start || is_stop) {
        const auto action = is_start ? MeetingInboundAction::Start : MeetingInboundAction::Stop;
        if (!doc.command_id_is_string || !doc.session_id_is_string) {
            return {action, MeetingControlError::InvalidType};
        }
        if (!IsMeetingUuid(doc.command_id) || !IsMeetingUuid(doc.session_id)) {
            return {action, MeetingControlError::InvalidUuid};
        }
        if (doc.mac_present) {
            return {action, MeetingControlError::WrongDirection};
        }
        if (is_start) {
            if (doc.root_field_count != 6 || doc.audio_field_count != 4) {
                return {action, MeetingControlError::InvalidField};
            }
            if (!doc.audio_is_object || !doc.codec_is_string || !doc.sample_rate_is_integer ||
                !doc.channels_is_integer || !doc.frame_duration_is_integer) {
                return {action, MeetingControlError::InvalidType};
            }
            if (!MeetingStringEquals(doc.codec, "opus") || doc.sample_rate != 16000 ||
                doc.channels != 1 || doc.frame_duration_ms != 60) {
                return {action, MeetingControlError::UnsupportedAudio};
            }
        } else {
            if (doc.root_field_count != 5 || doc.audio_is_object) {
                return {action, MeetingControlError::InvalidField};
            }
        }
        return {action, MeetingControlError::None};
    }

    if (MeetingStringEquals(doc.action, "meeting.error")) {
        const unsigned int expected_fields = 4U +
            (doc.command_id_is_string ? 1U : 0U) +
            (doc.session_id_is_string ? 1U : 0U);
        if (doc.root_field_count != expected_fields) {
            return {MeetingInboundAction::Error, MeetingControlError::InvalidField};
        }
        if (!doc.code_is_string || doc.code == nullptr || doc.code[0] == '\0') {
            return {MeetingInboundAction::Error, MeetingControlError::InvalidType};
        }
        if (!IsStableMeetingErrorCode(doc.code)) {
            return {MeetingInboundAction::Error, MeetingControlError::InvalidField};
        }
        if ((doc.command_id_is_string && !IsMeetingUuid(doc.command_id)) ||
            (doc.session_id_is_string && !IsMeetingUuid(doc.session_id))) {
            return {MeetingInboundAction::Error, MeetingControlError::InvalidUuid};
        }
        return {MeetingInboundAction::Error, MeetingControlError::None};
    }

    return {MeetingInboundAction::None, MeetingControlError::UnknownAction};
}

#endif
