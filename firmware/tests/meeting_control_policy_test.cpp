#include "meeting_control_policy.h"

namespace {

MeetingControlDocument valid_start()
{
    MeetingControlDocument doc;
    doc.payload_bytes = 256;
    doc.root_field_count = 6;
    doc.protocol_version_is_integer = true;
    doc.protocol_version = 1;
    doc.action_is_string = true;
    doc.action = "meeting.start";
    doc.message_id_is_string = true;
    doc.message_id = "00112233-4455-4677-8899-aabbccddeeff";
    doc.command_id_is_string = true;
    doc.command_id = "11112233-4455-4677-8899-aabbccddeeff";
    doc.session_id_is_string = true;
    doc.session_id = "22222233-4455-4677-8899-aabbccddeeff";
    doc.audio_is_object = true;
    doc.audio_field_count = 4;
    doc.codec_is_string = true;
    doc.codec = "opus";
    doc.sample_rate_is_integer = true;
    doc.sample_rate = 16000;
    doc.channels_is_integer = true;
    doc.channels = 1;
    doc.frame_duration_is_integer = true;
    doc.frame_duration_ms = 60;
    return doc;
}

int strict_schema_and_direction()
{
    auto doc = valid_start();
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::None) return __LINE__;
    doc.sample_rate = 48000;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::UnsupportedAudio) return __LINE__;
    doc = valid_start();
    doc.root_is_object = false;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::InvalidType) return __LINE__;
    doc = valid_start();
    doc.protocol_version_is_integer = false;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::InvalidType) return __LINE__;
    doc = valid_start();
    doc.action = "meeting.start-requested";
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::WrongDirection) return __LINE__;
    doc = valid_start();
    doc.protocol_version = 2;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::UnsupportedVersion) return __LINE__;
    doc = valid_start();
    doc.payload_bytes = 16385;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::PayloadTooLarge) return __LINE__;
    doc = valid_start();
    doc.root_field_count = 7;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::InvalidField) return __LINE__;
    doc = valid_start();
    doc.audio_field_count = 5;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::InvalidField) return __LINE__;
    doc = valid_start();
    doc.message_id = "not-a-uuid";
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::InvalidUuid) return __LINE__;
    return 0;
}

int selected_capability_is_exact()
{
    MeetingControlDocument doc;
    doc.payload_bytes = 128;
    doc.root_field_count = 4;
    doc.protocol_version_is_integer = true;
    doc.protocol_version = 1;
    doc.action_is_string = true;
    doc.action = "protocol.selected";
    doc.message_id_is_string = true;
    doc.message_id = "00112233-4455-4677-8899-aabbccddeeff";
    doc.capabilities_is_array = true;
    doc.capability_count = 1;
    doc.meeting_v1_capability_count = 1;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::None) return __LINE__;
    doc.capability_count = 2;
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::UnsupportedCapability) return __LINE__;
    return 0;
}

int meeting_error_code_is_stable()
{
    MeetingControlDocument doc;
    doc.payload_bytes = 128;
    doc.root_field_count = 6;
    doc.protocol_version_is_integer = true;
    doc.protocol_version = 1;
    doc.action_is_string = true;
    doc.action = "meeting.error";
    doc.message_id_is_string = true;
    doc.message_id = "00112233-4455-4677-8899-aabbccddeeff";
    doc.command_id_is_string = true;
    doc.command_id = "11112233-4455-4677-8899-aabbccddeeff";
    doc.session_id_is_string = true;
    doc.session_id = "22222233-4455-4677-8899-aabbccddeeff";
    doc.code_is_string = true;
    doc.code = "ARBITRARY_OLD_ERROR";
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::InvalidField) {
        return __LINE__;
    }
    doc.code = "DEVICE_ERROR";
    if (ValidateInboundMeetingControl(doc).error != MeetingControlError::None) return __LINE__;
    return 0;
}

}  // namespace

extern "C" int run_tests()
{
    if (const int result = strict_schema_and_direction()) return result;
    if (const int result = selected_capability_is_exact()) return result;
    return meeting_error_code_is_stable();
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
