/*
 * SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
 *
 * SPDX-License-Identifier: MIT
 */
#include "hal.h"
#include <stackchan/stackchan.h>
#include "board/hal_bridge.h"
#include <mooncake.h>
#include <mooncake_log.h>
#include <board.h>
#include <web_socket.h>
#include <esp_log.h>
#include <arpa/inet.h>
#include <jpg/image_to_jpeg.h>
#include <wifi_station.h>
#include <ArduinoJson.hpp>
#include <settings.h>
#include <mutex>
#include <deque>
#include <vector>
#include <atomic>
#include <condition_variable>
#include <esp_heap_caps.h>
#include <esp_system.h>
#include <display.h>
#include <lvgl_image.h>
#include <wifi_manager.h>
#include "utils/jpeg_to_image/jpeg_decoder.h"
#include "utils/secret_logic/secret_logic.h"
#include <stackchan/meeting_service_lifecycle_policy.h>
#include <stackchan/meeting_command_policy.h>
#include <stackchan/meeting_callback_gate_state.h>
#include <stackchan/meeting_send_queue_adapter.h>
#include <stackchan/meeting_callback_shutdown.h>

static std::string _tag = "WS-Avatar";

static const std::string _setting_ns              = "stackchan";
static const std::string _setting_device_name_key = "device_name";

namespace {

constexpr size_t kMaxInboundPayloadBytes = 2 * 1024 * 1024;
constexpr size_t kMaxInboundQueueMessages = 24;
constexpr size_t kMaxInboundQueueBytes = 4 * 1024 * 1024;
constexpr size_t kMaxMeetingControlBytes = 16 * 1024;
constexpr size_t kMaxMeetingSendFrames = 12;
constexpr size_t kMaxMeetingSendBytes = 64 * 1024;
constexpr uint64_t kMeetingPressureTimeoutMs = 2000;

uint64_t monotonicMilliseconds()
{
    return static_cast<uint64_t>(esp_timer_get_time() / 1000);
}

MeetingEnqueueDisposition_t rejectedDisposition(MeetingTransportError error)
{
    return {MeetingMessageDisposition::Rejected, error, 0, false, false};
}

std::string createMessageId()
{
    return fmt::format(
        "{:08x}-{:04x}-4{:03x}-{:04x}-{:08x}{:04x}",
        esp_random(),
        esp_random() & 0xFFFFU,
        esp_random() & 0x0FFFU,
        (esp_random() & 0x3FFFU) | 0x8000U,
        esp_random(),
        esp_random() & 0xFFFFU
    );
}

}  // namespace

class WebSocketAvatar {
public:
    WebSocketAvatar()
        : _callback_gate(std::make_shared<CallbackGate>(this))
    {
    }

    ~WebSocketAvatar()
    {
        shutdown();
    }

    void shutdown()
    {
        {
            std::unique_lock<std::mutex> shutdown_lock(_shutdown_mutex);
            if (_shutdown_complete) return;
            if (_shutdown_started) {
                _shutdown_cv.wait(shutdown_lock, [this]() { return _shutdown_complete; });
                return;
            }
            _shutdown_started = true;
        }
        ShutdownMeetingCallbacks(
            [this]() {
                {
                    std::lock_guard<std::mutex> gate_lock(_callback_gate->mutex);
                    _callback_gate->state.Disable();
                }
                if (_signals_connected) {
                    GetHAL().onWsCallResponse.disconnect(_call_response_connection);
                    GetHAL().onWsCallEnd.disconnect(_call_end_connection);
                }
            },
            [this]() {
                std::unique_lock<std::mutex> gate_lock(_callback_gate->mutex);
                _callback_gate->cv.wait(gate_lock, [this]() {
                    return _callback_gate->state.CanDestroy();
                });
            },
            [this]() {
                std::lock_guard<std::mutex> socket_lock(_socket_mutex);
                _websocket.reset();
            }
        );
        {
            std::lock_guard<std::mutex> shutdown_lock(_shutdown_mutex);
            _shutdown_complete = true;
        }
        _shutdown_cv.notify_all();
    }

    enum class DataType : uint8_t {
        Opus              = 0x01,
        Jpeg              = 0x02,
        ControlAvatar     = 0x03,
        ControlMotion     = 0x04,
        StartCameraStream = 0x05,
        StopCameraStream  = 0x06,
        TextMessage       = 0x07,
        RequestCall       = 0x09,
        DeclineCall       = 0x0A,
        AcceptCall        = 0x0B,
        EndCall           = 0x0C,
        SetDeviceName     = 0x0D,
        GetDeviceName     = 0x0E,
        HeartbeatPing     = 0x10,
        HeartbeatPong     = 0x11,
        VideoModeOn       = 0x12,
        VideoModeOff      = 0x13,
        DanceSequence     = 0x14,
        StartAudioStream  = 0x18,
        StopAudioStream   = 0x19,
        MeetingControl    = 0x1B,
    };

    struct ReceivedMessage {
        bool binary;
        std::vector<uint8_t> data;
    };

    void init()
    {
        _url = stackchan::meeting::buildWebSocketUrl(secret_logic::get_server_url());
        if (_url.empty()) {
            ESP_LOGE(_tag.c_str(), "Invalid StackChan server URL");
            noteMeetingFailure(MeetingTransportError::ServiceUnavailable);
            return;
        }

        connect();

        const std::weak_ptr<CallbackGate> weak_gate = _callback_gate;
        _call_response_connection = GetHAL().onWsCallResponse.connect([weak_gate](bool accepted) {
            runCurrentCallback(weak_gate, [accepted](WebSocketAvatar& owner) {
                if (!owner.isConnected()) return;
                if (accepted) {
                    ESP_LOGI(_tag.c_str(), "Sending AcceptCall");
                    owner.sendPacket(DataType::AcceptCall, nullptr, 0);
                } else {
                    ESP_LOGI(_tag.c_str(), "Sending DeclineCall");
                    owner.sendPacket(DataType::DeclineCall, nullptr, 0);
                }
            });
        });

        _call_end_connection = GetHAL().onWsCallEnd.connect([weak_gate](WsSignalSource source) {
            runCurrentCallback(weak_gate, [source](WebSocketAvatar& owner) {
                if (!owner.isConnected() || source != WsSignalSource::Local) return;
                ESP_LOGI(_tag.c_str(), "Sending EndCall");
                owner.sendPacket(DataType::EndCall, nullptr, 0);
            });
        });
        _signals_connected = true;
    }

    void connect()
    {
        auto device_credential = secret_logic::get_device_credential();
        auto token = device_credential.empty()
                         ? secret_logic::generate_auth_token()
                         : "Device " + device_credential;

        unsigned long long connection_generation = 0;
        {
            std::unique_lock<std::mutex> gate_lock(_callback_gate->mutex);
            _callback_gate->cv.wait(gate_lock, [this]() {
                return _callback_gate->state.CanDestroy();
            });
            connection_generation = _callback_gate->state.BeginGeneration();
        }

        // Invalidate callbacks from the old socket before destroying it.
        {
            std::lock_guard<std::mutex> socket_lock(_socket_mutex);
            _websocket.reset();
        }

        auto& board  = Board::GetInstance();
        auto network = board.GetNetwork();

        // 创建 WebSocket 实例
        auto created_websocket = network->CreateWebSocket(1);
        std::shared_ptr<WebSocket> websocket(std::move(created_websocket));

        if (!websocket) {
            ESP_LOGE(_tag.c_str(), "Failed to create websocket");
            return;
        }

        // 设置认证头
        websocket->SetHeader("Authorization", token.c_str());

        // 设置回调
        const std::weak_ptr<CallbackGate> weak_gate = _callback_gate;
        websocket->OnConnected([weak_gate, connection_generation]() {
            runGenerationCallback(weak_gate, connection_generation, [](WebSocketAvatar& owner) {
                ESP_LOGI(_tag.c_str(), "Connected to server!");
                owner._last_heartbeat_time = GetHAL().millis();
                owner.setConnectionState(true);
                owner.sendProtocolHello();
            });
        });

        websocket->OnDisconnected([weak_gate, connection_generation]() {
            runGenerationCallback(weak_gate, connection_generation, [](WebSocketAvatar& owner) {
                ESP_LOGI(_tag.c_str(), "Disconnected!");
                owner.setConnectionState(false);
            });
        });

        websocket->OnData([weak_gate, connection_generation](const char* data, size_t len, bool binary) {
            runGenerationCallback(weak_gate, connection_generation, [data, len, binary](WebSocketAvatar& owner) {
                std::lock_guard<std::mutex> lock(owner._mutex);
                if (data == nullptr || len > kMaxInboundPayloadBytes + 5 ||
                    owner._msg_queue.size() >= kMaxInboundQueueMessages ||
                    owner._msg_queue_bytes + len > kMaxInboundQueueBytes) {
                    ESP_LOGE(_tag.c_str(), "Dropping oversized/full WebSocket receive queue");
                    return;
                }
                owner._msg_queue.push_back({binary, std::vector<uint8_t>(data, data + len)});
                owner._msg_queue_bytes += len;
            });
        });

        // ESP_LOGI(_tag.c_str(), "Connecting to %s...", _url.c_str());
        // GetHAL().onWsLog.emit(CommonLogLevel::Info, "Connecting to server...");
        {
            std::lock_guard<std::mutex> socket_lock(_socket_mutex);
            _websocket = websocket;
        }
        if (!websocket->Connect(_url.c_str())) {
            ESP_LOGE(_tag.c_str(), "Failed to connect");
            setConnectionState(false);
            GetHAL().onWsLog.emit(CommonLogLevel::Error, "Connect to server Failed");
        }
        _last_reconnect_attempt = GetHAL().millis();
    }

    void update()
    {
        auto websocket = socketSnapshot();
        if (!websocket) {
            return;
        }

        if (!websocket->IsConnected()) {
            if (GetHAL().millis() - _last_reconnect_attempt > 5000) {
                connect();
            }
        } else {
            processMessages();
            flushMeetingQueue();

            // Check heartbeat timeout
            if (GetHAL().millis() - _last_heartbeat_time > 10000) {
                ESP_LOGE(_tag.c_str(), "Heartbeat timeout!");
                GetHAL().onWsLog.emit(CommonLogLevel::Error, "Heartbeat Timeout");
                _last_heartbeat_time = GetHAL().millis();
                return;
            }
        }

        if (_is_streaming) {
            if (GetHAL().millis() - _last_capture_time >= (_is_video_mode ? 700 : 350)) {
                captureAndSendFrame();
                _last_capture_time = GetHAL().millis();
            }
        }
    }

    void processMessages()
    {
        std::vector<ReceivedMessage> messages;
        {
            std::lock_guard<std::mutex> lock(_mutex);
            while (!_msg_queue.empty()) {
                _msg_queue_bytes -= _msg_queue.front().data.size();
                messages.push_back(std::move(_msg_queue.front()));
                _msg_queue.pop_front();
            }
        }

        for (const auto& msg : messages) {
            handleMessage(msg);
        }
    }

    void handleMessage(const ReceivedMessage& msg)
    {
        if (msg.binary) {
            const auto outer = stackchan::meeting::parseOuterFrame(
                msg.data.data(), msg.data.size(), kMaxInboundPayloadBytes
            );
            if (outer.error != stackchan::meeting::OuterFrameError::None) {
                ESP_LOGE(_tag.c_str(), "Rejected malformed WebSocket frame: %d", (int)outer.error);
                return;
            }
            DataType type = static_cast<DataType>(outer.type);
            ESP_LOGI(_tag.c_str(), "Received binary type: %d, len: %d", (int)type, (int)msg.data.size());

            switch (type) {
                // Opus handled in OnData Fast Path
                // case DataType::Opus: {
                //     if (msg.data.size() > 5) {
                //         auto packet = std::make_unique<AudioStreamPacket>();
                //         packet->payload.assign(msg.data.begin() + 5, msg.data.end());
                //         _audio_service.PushPacketToDecodeQueue(std::move(packet));
                //     }
                //     break;
                // }
                case DataType::StartCameraStream: {
                    ESP_LOGI(_tag.c_str(), "Start Camera Stream");
                    setStreamingEnabled(true);
                    if (auto websocket = socketSnapshot()) websocket->Send("camera stream started");
                    break;
                }
                case DataType::StopCameraStream: {
                    ESP_LOGI(_tag.c_str(), "Stop Camera Stream");
                    setStreamingEnabled(false);
                    if (auto websocket = socketSnapshot()) websocket->Send("camera stream stopped");
                    break;
                }
                case DataType::ControlAvatar: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        // ESP_LOGI(_tag.c_str(), "Control Avatar Payload: %s", payload.c_str());
                        GetHAL().onWsAvatarData.emit(payload);
                    }
                    break;
                }
                case DataType::ControlMotion: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        // ESP_LOGI(_tag.c_str(), "Control Motion Payload: %s", payload.c_str());
                        GetHAL().onWsMotionData.emit(payload);
                    }
                    break;
                }
                case DataType::RequestCall: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        ESP_LOGI(_tag.c_str(), "RequestCall Payload: %s", payload.c_str());
                        GetHAL().onWsCallRequest.emit(payload);
                    }
                    break;
                }
                case DataType::EndCall: {
                    ESP_LOGI(_tag.c_str(), "EndCall");
                    GetHAL().onWsCallEnd.emit(WsSignalSource::Remote);
                    break;
                }
                case DataType::SetDeviceName: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        ESP_LOGI(_tag.c_str(), "SetDeviceName Payload: %s", payload.c_str());

                        Settings settings(_setting_ns, true);
                        settings.SetString(_setting_device_name_key, payload);
                    }
                    break;
                }
                case DataType::GetDeviceName: {
                    ESP_LOGI(_tag.c_str(), "GetDeviceName");

                    Settings settings(_setting_ns, false);
                    auto device_name = settings.GetString(_setting_device_name_key, "StackChan");

                    sendPacket(DataType::GetDeviceName, (const uint8_t*)device_name.c_str(), device_name.size());
                    break;
                }
                case DataType::HeartbeatPing: {
                    ESP_LOGI(_tag.c_str(), "HeartbeatPing");
                    _last_heartbeat_time = GetHAL().millis();
                    sendPacket(DataType::HeartbeatPong, nullptr, 0);
                    break;
                }
                case DataType::TextMessage: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        ESP_LOGI(_tag.c_str(), "TextMessage Payload: %s", payload.c_str());

                        ArduinoJson::JsonDocument doc;
                        auto error = ArduinoJson::deserializeJson(doc, payload);
                        if (error) {
                            ESP_LOGE(_tag.c_str(), "DeserializeJson failed: %s", error.c_str());
                            return;
                        }

                        WsTextMessage_t text_msg;

                        if (doc["name"].is<std::string>()) {
                            text_msg.name = doc["name"].as<std::string>();
                        }
                        if (doc["content"].is<std::string>()) {
                            text_msg.content = doc["content"].as<std::string>();
                        }

                        GetHAL().onWsTextMessage.emit(text_msg);
                    }
                    break;
                }
                case DataType::VideoModeOn: {
                    ESP_LOGI(_tag.c_str(), "VideoModeOn");
                    GetHAL().onWsVideoModeChange.emit(true);
                    _is_video_mode = true;
                    break;
                }
                case DataType::VideoModeOff: {
                    ESP_LOGI(_tag.c_str(), "VideoModeOff");
                    GetHAL().onWsVideoModeChange.emit(false);
                    _is_video_mode = false;
                    break;
                }
                case DataType::Jpeg: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        ESP_LOGI(_tag.c_str(), "Jpeg Frame Received, size: %d", (int)(msg.data.size() - 5));

                        static int64_t _time_count = 0;
                        static int64_t _interval   = 0;
                        _time_count                = esp_timer_get_time();

                        size_t jpeg_len    = msg.data.size() - 5;
                        uint8_t* jpeg_data = (uint8_t*)heap_caps_malloc(jpeg_len, MALLOC_CAP_8BIT);
                        if (jpeg_data) {
                            memcpy(jpeg_data, msg.data.data() + 5, jpeg_len);

                            auto image = jpeg_dec::decode_to_lvgl(jpeg_data, jpeg_len);
                            if (image) {
                                // ESP_LOGI(_tag.c_str(), "Done");

                                _interval = esp_timer_get_time() - _time_count;
                                mclog::info("jpeg decode time: {} ms", _interval / 1000);

                                GetHAL().onWsVideoFrame.emit(image);
                            } else {
                                ESP_LOGE(_tag.c_str(), "Failed to decode JPEG");
                            }
                            heap_caps_free(jpeg_data);
                        } else {
                            ESP_LOGE(_tag.c_str(), "Failed to allocate memory for JPEG");
                        }
                    }
                    break;
                }
                case DataType::DanceSequence: {
                    // Protocol: [Type(1)] [Length(4)] [Payload]
                    if (msg.data.size() >= 5) {
                        std::string payload(msg.data.begin() + 5, msg.data.end());
                        // ESP_LOGI(_tag.c_str(), "Dance Payload:\n%s", payload.c_str());
                        ESP_LOGI(_tag.c_str(), "DanceSequence size: %d", (int)payload.size());
                        GetHAL().onWsDanceData.emit(payload);
                    }
                    break;
                }
                case DataType::StartAudioStream: {
                    break;
                }
                case DataType::StopAudioStream: {
                    break;
                }
                case DataType::MeetingControl: {
                    handleMeetingControl(outer.payload);
                    break;
                }
                default:
                    break;
            }
        } else {
            ESP_LOGI(_tag.c_str(), "Received text: %.*s", (int)msg.data.size(), (char*)msg.data.data());
        }
    }

    bool isConnected()
    {
        auto websocket = socketSnapshot();
        return websocket && websocket->IsConnected();
    }

    void captureAndSendFrame()
    {
        if (!isConnected()) {
            return;
        }

        static int64_t _time_count = 0;
        static int64_t _interval   = 0;

        auto camera = hal_bridge::board_get_camera();
        if (!camera) {
            return;
        }

        _time_count = esp_timer_get_time();
        if (camera->StreamCaptures()) {
            _interval = esp_timer_get_time() - _time_count;
            mclog::info("camera capture time: {} ms", _interval / 1000);

            const uint8_t* frameData = camera->GetFrameData();
            size_t frameSize         = camera->GetFrameSize();
            int width                = camera->GetFrameWidth();
            int height               = camera->GetFrameHeight();
            int format               = camera->GetFrameFormat();

            uint8_t* jpeg_data = nullptr;
            size_t jpeg_len    = 0;

            // 压缩为 JPEG
            _time_count = esp_timer_get_time();
            if (image_to_jpeg((uint8_t*)frameData, frameSize, width, height, (v4l2_pix_fmt_t)format, 20, &jpeg_data,
                              &jpeg_len)) {
                _interval = esp_timer_get_time() - _time_count;
                // mclog::info("jpeg encode time: {} ms, size: {}", _interval / 1000, jpeg_len);
                mclog::info("jpeg encode time: {} ms", _interval / 1000);

                if (jpeg_data) {
                    sendPacket(DataType::Jpeg, jpeg_data, jpeg_len);  // Type 2 for JPEG
                    free(jpeg_data);
                }
            }
        }
    }

    void setStreamingEnabled(bool enabled)
    {
        _is_streaming = enabled;
    }

    MeetingEnqueueDisposition_t enqueueMeetingAudio(const stackchan::meeting::AudioFrame& frame)
    {
        auto payload = stackchan::meeting::encodeAudioEnvelope(frame);
        if (payload.empty()) {
            return rejectedDisposition(MeetingTransportError::InvalidPayload);
        }
        return enqueueMeetingPayload(DataType::Opus, std::move(payload), true, frame.sequence);
    }

    MeetingEnqueueDisposition_t enqueueMeetingControl(std::string_view json)
    {
        if (json.empty() || json.size() > kMaxMeetingControlBytes) {
            return rejectedDisposition(MeetingTransportError::InvalidPayload);
        }
        return enqueueMeetingPayload(
            DataType::MeetingControl,
            std::vector<uint8_t>(json.begin(), json.end()),
            false,
            0
        );
    }

    MeetingTransportSnapshot_t meetingSnapshot()
    {
        std::lock_guard<std::mutex> lock(_meeting_mutex);
        const auto state = _meeting_transport.Snapshot(monotonicMilliseconds());
        MeetingTransportSnapshot_t snapshot;
        snapshot.connected = state.connected;
        snapshot.protocolSelected = state.protocol_selected;
        snapshot.queuedFrames = state.queued_messages;
        snapshot.queuedBytes = state.queued_bytes;
        snapshot.hasLocalSocketAcceptedSequence = state.has_local_socket_accepted_sequence;
        snapshot.lastLocalSocketAcceptedSequence = state.last_local_socket_accepted_sequence;
        snapshot.lastLocalSocketAcceptedTicket = state.last_local_socket_accepted_ticket;
        snapshot.pressureActive = state.pressure_active;
        snapshot.firstFailureMs = state.first_failure_ms;
        snapshot.failureLatched = state.failure_latched;
        snapshot.error = state.error;
        snapshot.pressureCause = state.pressure_cause;
        return snapshot;
    }

    bool beginMeetingSession()
    {
        std::lock_guard<std::mutex> flush_lock(_meeting_flush_mutex);
        std::lock_guard<std::mutex> lock(_meeting_mutex);
        const auto before_reset = _meeting_transport.Snapshot(monotonicMilliseconds());
        if (before_reset.failure_latched) {
            _meeting_transport.DiscardQueuedAfterTerminal();
            _meeting_send_queue.clear();
        }
        if (!_meeting_send_queue.empty()) {
            return false;
        }
        _meeting_transport.ResetForNewSession(monotonicMilliseconds());
        const auto state = _meeting_transport.Snapshot(monotonicMilliseconds());
        return state.connected && state.protocol_selected && !state.failure_latched;
    }

    void serviceMeetingTransport() { flushMeetingQueue(); }

    void markMeetingCommandStopped(std::string_view session_id, std::string_view command_id)
    {
        std::lock_guard<std::mutex> lock(_meeting_mutex);
        const std::string session(session_id);
        const std::string command(command_id);
        _meeting_command_policy.MarkStopped(
            MeetingCommandKeyFromUuid(session.c_str()),
            MeetingCommandKeyFromUuid(command.c_str())
        );
        _meeting_command_active = false;
        _meeting_active_session.clear();
        _meeting_start_command.clear();
        _meeting_stop_command.clear();
    }

    void abortMeetingCommandState()
    {
        std::lock_guard<std::mutex> lock(_meeting_mutex);
        _meeting_command_policy.Abort();
        _meeting_command_active = false;
        _meeting_active_session.clear();
        _meeting_start_command.clear();
        _meeting_stop_command.clear();
    }

    void markMeetingCommandStartFailed(std::string_view session_id, std::string_view command_id)
    {
        std::lock_guard<std::mutex> lock(_meeting_mutex);
        const std::string session(session_id);
        const std::string command(command_id);
        _meeting_command_policy.MarkStartFailed(
            MeetingCommandKeyFromUuid(session.c_str()),
            MeetingCommandKeyFromUuid(command.c_str())
        );
        _meeting_command_active = false;
        _meeting_active_session.clear();
        _meeting_start_command.clear();
        _meeting_stop_command.clear();
    }

private:
    struct CallbackGate {
        explicit CallbackGate(WebSocketAvatar* owner)
            : state(owner)
        {
        }

        std::mutex mutex;
        std::condition_variable cv;
        MeetingCallbackGateState state;
    };

    template <typename Callback>
    static void runGenerationCallback(
        const std::weak_ptr<CallbackGate>& weak_gate,
        unsigned long long generation,
        Callback&& callback
    ) {
        auto gate = weak_gate.lock();
        if (!gate) return;
        std::unique_lock<std::mutex> gate_lock(gate->mutex);
        void* raw_owner = nullptr;
        if (!gate->state.TryEnter(generation, raw_owner)) return;
        gate_lock.unlock();
        callback(*static_cast<WebSocketAvatar*>(raw_owner));
        gate_lock.lock();
        gate->state.Leave();
        if (gate->state.CanDestroy()) gate->cv.notify_all();
    }

    template <typename Callback>
    static void runCurrentCallback(
        const std::weak_ptr<CallbackGate>& weak_gate,
        Callback&& callback
    ) {
        auto gate = weak_gate.lock();
        if (!gate) return;
        std::unique_lock<std::mutex> gate_lock(gate->mutex);
        void* raw_owner = nullptr;
        if (!gate->state.TryEnterCurrent(raw_owner)) return;
        gate_lock.unlock();
        callback(*static_cast<WebSocketAvatar*>(raw_owner));
        gate_lock.lock();
        gate->state.Leave();
        if (gate->state.CanDestroy()) gate->cv.notify_all();
    }

    struct MeetingOutgoingMessage {
        DataType type;
        std::vector<uint8_t> payload;
        bool has_sequence = false;
        uint32_t sequence = 0;
        uint64_t ticket = 0;
    };

    mutable std::mutex _socket_mutex;
    std::shared_ptr<WebSocket> _websocket;
    std::shared_ptr<CallbackGate> _callback_gate;
    size_t _call_response_connection = 0;
    size_t _call_end_connection = 0;
    bool _signals_connected = false;
    std::mutex _shutdown_mutex;
    std::condition_variable _shutdown_cv;
    bool _shutdown_started = false;
    bool _shutdown_complete = false;
    std::string _url;
    uint32_t _last_reconnect_attempt = 0;
    uint32_t _last_capture_time      = 0;
    uint32_t _last_heartbeat_time    = 0;
    bool _is_streaming               = false;
    bool _is_video_mode              = false;
    std::mutex _mutex;
    std::deque<ReceivedMessage> _msg_queue;
    size_t _msg_queue_bytes = 0;

    std::mutex _send_mutex;
    std::mutex _meeting_mutex;
    std::mutex _meeting_flush_mutex;
    std::deque<MeetingOutgoingMessage> _meeting_send_queue;
    MeetingTransportPolicy _meeting_transport{
        kMaxMeetingSendFrames,
        kMaxMeetingSendBytes,
        kMeetingPressureTimeoutMs,
    };
    MeetingCommandPolicy _meeting_command_policy;
    bool _meeting_command_active = false;
    std::string _meeting_active_session;
    std::string _meeting_start_command;
    std::string _meeting_stop_command;

    std::shared_ptr<WebSocket> socketSnapshot() const
    {
        std::lock_guard<std::mutex> socket_lock(_socket_mutex);
        return _websocket;
    }

    void noteMeetingFailure(MeetingTransportError failure)
    {
        std::lock_guard<std::mutex> lock(_meeting_mutex);
        _meeting_transport.NoteFailure(failure, monotonicMilliseconds());
    }

    void setConnectionState(bool connected)
    {
        {
            std::lock_guard<std::mutex> lock(_meeting_mutex);
            _meeting_transport.SetConnection(connected, monotonicMilliseconds());
            _meeting_transport.SetProtocolSelected(false, monotonicMilliseconds());
        }
        GetHAL().onWsConnectionChanged.emit(connected);
        if (!connected) {
            GetHAL().onWsMeetingProtocolSelected.emit(false);
        }
    }

    void sendProtocolHello()
    {
        ArduinoJson::JsonDocument doc;
        doc["protocolVersion"] = 1;
        doc["action"] = "protocol.hello";
        doc["messageId"] = createMessageId();
        doc["role"] = "device";
        auto capabilities = doc["capabilities"].to<ArduinoJson::JsonArray>();
        capabilities.add("meeting-v1");

        std::string json;
        ArduinoJson::serializeJson(doc, json);
        if (!sendPacket(DataType::MeetingControl, reinterpret_cast<const uint8_t*>(json.data()), json.size())) {
            noteMeetingFailure(MeetingTransportError::SendFailed);
        }
    }

    void emitMeetingControlError(
        MeetingControlError error,
        const ArduinoJson::JsonDocument* doc = nullptr,
        const char* response_code = nullptr
    ) {
        MeetingEvent_t event;
        event.kind = MeetingEventKind::ControlRejected;
        event.controlError = error;
        if (doc != nullptr) {
            if ((*doc)["messageId"].is<const char*>()) {
                event.messageId = (*doc)["messageId"].as<std::string>();
            }
            if ((*doc)["commandId"].is<const char*>()) {
                event.commandId = (*doc)["commandId"].as<std::string>();
            }
            if ((*doc)["sessionId"].is<const char*>()) {
                event.sessionId = (*doc)["sessionId"].as<std::string>();
            }
        }
        ArduinoJson::JsonDocument response;
        response["protocolVersion"] = 1;
        response["action"] = "meeting.error";
        response["messageId"] = createMessageId();
        if (IsMeetingUuid(event.messageId.c_str())) {
            response["correlationMessageId"] = normalizedUuid(event.messageId.c_str());
        }
        if (IsMeetingUuid(event.commandId.c_str())) {
            response["commandId"] = normalizedUuid(event.commandId.c_str());
        }
        if (IsMeetingUuid(event.sessionId.c_str())) {
            response["sessionId"] = normalizedUuid(event.sessionId.c_str());
        }
        response["code"] = response_code != nullptr
            ? response_code
            : (error == MeetingControlError::UnsupportedVersion ||
                       error == MeetingControlError::UnsupportedCapability
                   ? "PROTOCOL_UNSUPPORTED"
                   : "DEVICE_ERROR");
        std::string response_json;
        ArduinoJson::serializeJson(response, response_json);
        event.responseDisposition = enqueueMeetingControl(response_json);
        GetHAL().onMeetingEvent.emit(event);
    }

    static std::string normalizedUuid(const char* value)
    {
        std::string normalized = value == nullptr ? "" : value;
        for (char& character : normalized) {
            if (character >= 'A' && character <= 'F') {
                character = static_cast<char>(character - 'A' + 'a');
            }
        }
        return normalized;
    }

    void handleMeetingControl(const std::vector<uint8_t>& payload)
    {
        if (payload.empty() || payload.size() > kMaxMeetingControlBytes) {
            emitMeetingControlError(MeetingControlError::PayloadTooLarge);
            return;
        }

        ArduinoJson::JsonDocument doc;
        const auto error = ArduinoJson::deserializeJson(doc, payload.data(), payload.size());
        if (error) {
            ESP_LOGE(_tag.c_str(), "Rejected invalid meeting control payload");
            emitMeetingControlError(MeetingControlError::MalformedJson);
            return;
        }

        MeetingControlDocument schema;
        schema.payload_bytes = payload.size();
        schema.root_is_object = doc.is<ArduinoJson::JsonObject>();
        schema.root_field_count = doc.as<ArduinoJson::JsonObject>().size();
        schema.protocol_version_is_integer = doc["protocolVersion"].is<int>() &&
            !doc["protocolVersion"].is<bool>();
        schema.protocol_version = doc["protocolVersion"].as<int>();
        schema.action_is_string = doc["action"].is<const char*>();
        schema.action = doc["action"].as<const char*>();
        schema.message_id_is_string = doc["messageId"].is<const char*>();
        schema.message_id = doc["messageId"].as<const char*>();
        schema.command_id_is_string = doc["commandId"].is<const char*>();
        schema.command_id = doc["commandId"].as<const char*>();
        schema.session_id_is_string = doc["sessionId"].is<const char*>();
        schema.session_id = doc["sessionId"].as<const char*>();
        schema.capabilities_is_array = doc["capabilities"].is<ArduinoJson::JsonArray>();
        if (schema.capabilities_is_array) {
            const auto capabilities = doc["capabilities"].as<ArduinoJson::JsonArray>();
            schema.capability_count = capabilities.size();
            for (ArduinoJson::JsonVariant capability : capabilities) {
                if (capability.is<const char*>() &&
                    capability.as<std::string>() == "meeting-v1") {
                    ++schema.meeting_v1_capability_count;
                }
            }
        }
        schema.audio_is_object = doc["audio"].is<ArduinoJson::JsonObject>();
        if (schema.audio_is_object) {
            schema.audio_field_count = doc["audio"].as<ArduinoJson::JsonObject>().size();
        }
        schema.codec_is_string = doc["audio"]["codec"].is<const char*>();
        schema.codec = doc["audio"]["codec"].as<const char*>();
        schema.sample_rate_is_integer = doc["audio"]["sampleRate"].is<int>() &&
            !doc["audio"]["sampleRate"].is<bool>();
        schema.sample_rate = doc["audio"]["sampleRate"].as<int>();
        schema.channels_is_integer = doc["audio"]["channels"].is<int>() &&
            !doc["audio"]["channels"].is<bool>();
        schema.channels = doc["audio"]["channels"].as<int>();
        schema.frame_duration_is_integer = doc["audio"]["frameDurationMs"].is<int>() &&
            !doc["audio"]["frameDurationMs"].is<bool>();
        schema.frame_duration_ms = doc["audio"]["frameDurationMs"].as<int>();
        schema.code_is_string = doc["code"].is<const char*>();
        schema.code = doc["code"].as<const char*>();
        schema.mac_present = !doc["mac"].isNull();

        const auto validation = ValidateInboundMeetingControl(schema);
        if (validation.error != MeetingControlError::None) {
            ESP_LOGE(_tag.c_str(), "Rejected meeting control schema: %d", (int)validation.error);
            emitMeetingControlError(validation.error, &doc);
            return;
        }

        if (validation.action == MeetingInboundAction::ProtocolSelected) {
            {
                std::lock_guard<std::mutex> lock(_meeting_mutex);
                _meeting_transport.SetProtocolSelected(true, monotonicMilliseconds());
            }
            GetHAL().onWsMeetingProtocolSelected.emit(true);
            return;
        }

        if (validation.action == MeetingInboundAction::Error) {
            const std::string session = schema.session_id_is_string
                ? normalizedUuid(schema.session_id)
                : std::string();
            const std::string command = schema.command_id_is_string
                ? normalizedUuid(schema.command_id)
                : std::string();
            bool correlated = false;
            {
                std::lock_guard<std::mutex> lock(_meeting_mutex);
                correlated = IsCorrelatedMeetingError(
                    _meeting_command_active,
                    MeetingCommandKeyFromUuid(_meeting_active_session.c_str()),
                    MeetingCommandKeyFromUuid(_meeting_start_command.c_str()),
                    !_meeting_stop_command.empty(),
                    MeetingCommandKeyFromUuid(_meeting_stop_command.c_str()),
                    schema.session_id_is_string,
                    MeetingCommandKeyFromUuid(session.c_str()),
                    schema.command_id_is_string,
                    MeetingCommandKeyFromUuid(command.c_str())
                );
            }
            if (!correlated) {
                emitMeetingControlError(MeetingControlError::InvalidField, &doc, "SESSION_BUSY");
                return;
            }
            MeetingEvent_t event;
            event.kind = MeetingEventKind::RemoteError;
            event.messageId = normalizedUuid(schema.message_id);
            if (schema.command_id_is_string) event.commandId = normalizedUuid(schema.command_id);
            if (schema.session_id_is_string) event.sessionId = normalizedUuid(schema.session_id);
            event.code = schema.code;
            GetHAL().onMeetingEvent.emit(event);
            return;
        }

        WsMeetingCommand_t command;
        command.action = validation.action;
        command.messageId = normalizedUuid(schema.message_id);
        command.commandId = normalizedUuid(schema.command_id);
        command.sessionId = normalizedUuid(schema.session_id);
        MeetingCommandDecision command_decision;
        {
            std::lock_guard<std::mutex> lock(_meeting_mutex);
            command_decision = command.action == MeetingInboundAction::Start
                ? _meeting_command_policy.OnStart(
                      MeetingCommandKeyFromUuid(command.sessionId.c_str()),
                      MeetingCommandKeyFromUuid(command.commandId.c_str())
                  )
                : _meeting_command_policy.OnStop(
                      MeetingCommandKeyFromUuid(command.sessionId.c_str()),
                      MeetingCommandKeyFromUuid(command.commandId.c_str())
                  );
            if (command_decision == MeetingCommandDecision::Accept) {
                if (command.action == MeetingInboundAction::Start) {
                    _meeting_command_active = true;
                    _meeting_active_session = command.sessionId;
                    _meeting_start_command = command.commandId;
                    _meeting_stop_command.clear();
                } else {
                    _meeting_stop_command = command.commandId;
                }
            }
        }
        if (command_decision == MeetingCommandDecision::Conflict) {
            emitMeetingControlError(MeetingControlError::InvalidField, &doc, "SESSION_BUSY");
            return;
        }
        command.duplicate = command_decision == MeetingCommandDecision::Duplicate;
        GetHAL().onWsMeetingCommand.emit(command);
    }

    MeetingEnqueueDisposition_t enqueueMeetingPayload(
        DataType type,
        std::vector<uint8_t>&& payload,
        bool has_sequence,
        uint32_t sequence
    ) {
        MeetingEnqueueDisposition_t admitted;
        {
            std::lock_guard<std::mutex> lock(_meeting_mutex);
            admitted = _meeting_transport.Admit(
                payload.size(), has_sequence, sequence, monotonicMilliseconds()
            );
            if (!admitted.enqueued) return admitted;
            _meeting_send_queue.push_back(
                {type, std::move(payload), has_sequence, sequence, admitted.ticket}
            );
        }

        flushMeetingQueue();
        std::lock_guard<std::mutex> lock(_meeting_mutex);
        const auto snapshot = _meeting_transport.Snapshot(monotonicMilliseconds());
        if (snapshot.last_local_socket_accepted_ticket >= admitted.ticket) {
            admitted.disposition = MeetingMessageDisposition::LocalSocketAccepted;
            admitted.locally_accepted = true;
            admitted.enqueued = false;
            return admitted;
        }
        if (snapshot.failure_latched) {
            admitted.disposition = MeetingMessageDisposition::TerminalFailure;
        }
        admitted.error = snapshot.error;
        return admitted;
    }

    void flushMeetingQueue()
    {
        std::lock_guard<std::mutex> flush_lock(_meeting_flush_mutex);
        for (size_t sent = 0; sent < 4; ++sent) {
            MeetingOutgoingMessage message;
            {
                std::lock_guard<std::mutex> lock(_meeting_mutex);
                if (!BeginMeetingQueueSend(_meeting_transport, _meeting_send_queue, message)) return;
            }

            const bool send_succeeded = sendPacket(
                message.type, message.payload.data(), message.payload.size()
            );
            bool emit_transport_failure = false;
            MeetingTransportError transport_failure = MeetingTransportError::None;
            {
                std::lock_guard<std::mutex> lock(_meeting_mutex);
                const auto disposition = FinishMeetingQueueSend(
                    _meeting_transport,
                    _meeting_send_queue,
                    message,
                    send_succeeded,
                    monotonicMilliseconds()
                );
                if (disposition.disposition == MeetingMessageDisposition::TerminalFailure) {
                    emit_transport_failure = true;
                    transport_failure = disposition.error;
                }
            }
            // Never emit into AppMeeting while holding _meeting_mutex: bridge
            // callbacks may enter HAL while holding the bridge mutex.
            if (emit_transport_failure) {
                MeetingEvent_t event;
                event.kind = MeetingEventKind::TransportFailure;
                event.transportError = transport_failure;
                GetHAL().onMeetingEvent.emit(event);
            }
            if (!send_succeeded) {
                return;
            }
        }
    }

    bool sendPacket(DataType type, const uint8_t* data, size_t len)
    {
        std::lock_guard<std::mutex> lock(_send_mutex);
        auto websocket = socketSnapshot();
        if (!websocket || !websocket->IsConnected()) {
            return false;
        }

        // mclog::info("sending packet type: {}, len: {}", (int)type, (int)len);

        // static int64_t _time_count = 0;
        // static int64_t _interval   = 0;
        // _time_count                = esp_timer_get_time();

        std::vector<uint8_t> packet;
        packet.reserve(1 + 4 + len);

        // [1 byte type]
        packet.push_back(static_cast<uint8_t>(type));

        // [4 bytes length] (Big Endian)
        uint32_t net_len       = htonl((uint32_t)len);
        const uint8_t* len_ptr = (const uint8_t*)&net_len;
        packet.push_back(len_ptr[0]);
        packet.push_back(len_ptr[1]);
        packet.push_back(len_ptr[2]);
        packet.push_back(len_ptr[3]);

        // [payload]
        if (len > 0) {
            packet.insert(packet.end(), data, data + len);
        }

        // _interval = esp_timer_get_time() - _time_count;
        // mclog::info("pack time: {} ms, size: {}", _interval / 1000, packet.size());

        // _time_count = esp_timer_get_time();
        return websocket->Send(packet.data(), packet.size(), true);
        // _interval = esp_timer_get_time() - _time_count;
        // mclog::info("send time: {} ms, size: {}", _interval / 1000, packet.size());
    }
};

namespace {

std::mutex websocket_avatar_service_mutex;
std::shared_ptr<WebSocketAvatar> websocket_avatar_service;
MeetingServiceLifecyclePolicy websocket_avatar_lifecycle;

std::shared_ptr<WebSocketAvatar> acquireWebSocketAvatarService()
{
    std::lock_guard<std::mutex> lock(websocket_avatar_service_mutex);
    if (!websocket_avatar_lifecycle.AcquireReady()) return {};
    return websocket_avatar_service;
}

}  // namespace

class WebsocketAvatarWorker : public mooncake::BasicAbility {
public:
    WebsocketAvatarWorker()
    {
        _service = std::make_shared<WebSocketAvatar>();
        _service->init();
        {
            std::lock_guard<std::mutex> lock(websocket_avatar_service_mutex);
            websocket_avatar_service = _service;
            websocket_avatar_lifecycle.PublishReady();
        }
    }

    void onCreate() override
    {
    }

    void onRunning() override
    {
        if (GetHAL().millis() - _last_tick < 20) {
            return;
        }
        _last_tick = GetHAL().millis();
        _service->update();
    }

    void onDestroy() override
    {
        {
            std::lock_guard<std::mutex> lock(websocket_avatar_service_mutex);
            websocket_avatar_lifecycle.BeginStop();
            websocket_avatar_service.reset();
        }
        _service->shutdown();
        _service.reset();
        {
            std::lock_guard<std::mutex> lock(websocket_avatar_service_mutex);
            websocket_avatar_lifecycle.PublishStopped();
        }
    }

private:
    std::shared_ptr<WebSocketAvatar> _service;
    uint32_t _last_tick = 0;
};

void Hal::startWebSocketAvatarService(std::function<void(std::string_view)> onStartLog)
{
    ensureWebSocketAvatarServiceStarted(std::move(onStartLog));
}

void Hal::ensureWebSocketAvatarServiceStarted(std::function<void(std::string_view)> onStartLog)
{
    {
        std::lock_guard<std::mutex> lock(websocket_avatar_service_mutex);
        if (!websocket_avatar_lifecycle.BeginEnsure()) return;
    }

    mclog::tagInfo(_tag, "ensure websocket avatar service started");

    startNetwork(onStartLog);

    if (onStartLog) {
        onStartLog("Connecting to\nserver...");
    }
    mooncake::GetMooncake().extensionManager()->createAbility(std::make_unique<WebsocketAvatarWorker>());
}

MeetingEnqueueDisposition_t Hal::enqueueMeetingOpusFrame(const stackchan::meeting::AudioFrame& frame)
{
    auto service = acquireWebSocketAvatarService();
    if (!service) return rejectedDisposition(MeetingTransportError::ServiceUnavailable);
    return service->enqueueMeetingAudio(frame);
}

MeetingEnqueueDisposition_t Hal::enqueueMeetingControl(std::string_view json)
{
    auto service = acquireWebSocketAvatarService();
    if (!service) return rejectedDisposition(MeetingTransportError::ServiceUnavailable);
    return service->enqueueMeetingControl(json);
}

MeetingTransportSnapshot_t Hal::getMeetingTransportSnapshot()
{
    auto service = acquireWebSocketAvatarService();
    if (!service) {
        MeetingTransportSnapshot_t snapshot;
        snapshot.error = MeetingTransportError::ServiceUnavailable;
        return snapshot;
    }
    return service->meetingSnapshot();
}

bool Hal::beginMeetingTransportSession()
{
    auto service = acquireWebSocketAvatarService();
    return service && service->beginMeetingSession();
}

void Hal::serviceMeetingTransport()
{
    auto service = acquireWebSocketAvatarService();
    if (service) service->serviceMeetingTransport();
}

void Hal::markMeetingCommandStopped(std::string_view session_id, std::string_view command_id)
{
    auto service = acquireWebSocketAvatarService();
    if (service) service->markMeetingCommandStopped(session_id, command_id);
}

void Hal::abortMeetingCommandState()
{
    auto service = acquireWebSocketAvatarService();
    if (service) service->abortMeetingCommandState();
}

void Hal::markMeetingCommandStartFailed(
    std::string_view session_id,
    std::string_view command_id
)
{
    auto service = acquireWebSocketAvatarService();
    if (service) service->markMeetingCommandStartFailed(session_id, command_id);
}
