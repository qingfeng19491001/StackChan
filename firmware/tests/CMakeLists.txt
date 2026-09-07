cmake_minimum_required(VERSION 3.16)

project(stackchan_firmware_tests LANGUAGES CXX)

enable_testing()

add_executable(motion_math_test
    motion_math_test.cpp
    ../main/stackchan/motion/motion_math.cpp
)

target_include_directories(motion_math_test PRIVATE
    ../main
)

target_compile_features(motion_math_test PRIVATE cxx_std_17)

add_test(NAME motion_math_test COMMAND $<TARGET_FILE:motion_math_test>)

add_executable(meeting_protocol_test
    meeting_protocol_test.cpp
    ../main/stackchan/meeting_protocol.cpp
)

target_include_directories(meeting_protocol_test PRIVATE
    ../main
)

target_compile_features(meeting_protocol_test PRIVATE cxx_std_17)

add_test(NAME meeting_protocol_test COMMAND $<TARGET_FILE:meeting_protocol_test>)

add_executable(audio_lifecycle_state_test
    audio_lifecycle_state_test.cpp
)

target_include_directories(audio_lifecycle_state_test PRIVATE
    ../xiaozhi-esp32/main/audio
)

target_compile_features(audio_lifecycle_state_test PRIVATE cxx_std_17)

add_test(NAME audio_lifecycle_state_test COMMAND $<TARGET_FILE:audio_lifecycle_state_test>)

add_executable(pcm_frame_accumulator_test
    pcm_frame_accumulator_test.cpp
)

target_include_directories(pcm_frame_accumulator_test PRIVATE
    ../xiaozhi-esp32/main/audio
)

target_compile_features(pcm_frame_accumulator_test PRIVATE cxx_std_17)

add_test(NAME pcm_frame_accumulator_test COMMAND $<TARGET_FILE:pcm_frame_accumulator_test>)

add_executable(audio_runtime_state_test
    audio_runtime_state_test.cpp
)

target_include_directories(audio_runtime_state_test PRIVATE
    ../xiaozhi-esp32/main/audio
)

target_compile_features(audio_runtime_state_test PRIVATE cxx_std_17)

add_test(NAME audio_runtime_state_test COMMAND $<TARGET_FILE:audio_runtime_state_test>)

add_executable(meeting_lifecycle_policy_test
    meeting_lifecycle_policy_test.cpp
)

target_include_directories(meeting_lifecycle_policy_test PRIVATE
    ../main/stackchan
)

target_compile_features(meeting_lifecycle_policy_test PRIVATE cxx_std_17)

add_test(NAME meeting_lifecycle_policy_test COMMAND $<TARGET_FILE:meeting_lifecycle_policy_test>)

add_executable(audio_lifecycle_policy_test
    audio_lifecycle_policy_test.cpp
)

target_include_directories(audio_lifecycle_policy_test PRIVATE
    ../xiaozhi-esp32/main/audio
)

target_compile_features(audio_lifecycle_policy_test PRIVATE cxx_std_17)

add_test(NAME audio_lifecycle_policy_test COMMAND $<TARGET_FILE:audio_lifecycle_policy_test>)

add_executable(pcm_tail_policy_test
    pcm_tail_policy_test.cpp
)

target_include_directories(pcm_tail_policy_test PRIVATE
    ../xiaozhi-esp32/main/audio
)

target_compile_features(pcm_tail_policy_test PRIVATE cxx_std_17)

add_test(NAME pcm_tail_policy_test COMMAND $<TARGET_FILE:pcm_tail_policy_test>)

add_executable(audio_drain_barrier_policy_test
    audio_drain_barrier_policy_test.cpp
)

target_include_directories(audio_drain_barrier_policy_test PRIVATE
    ../xiaozhi-esp32/main/audio
)

target_compile_features(audio_drain_barrier_policy_test PRIVATE cxx_std_17)

add_test(NAME audio_drain_barrier_policy_test COMMAND $<TARGET_FILE:audio_drain_barrier_policy_test>)

foreach(policy_test IN ITEMS
    launcher_selection_policy_test
    meeting_transport_policy_test
    meeting_command_policy_test
    meeting_service_lifecycle_policy_test
    meeting_control_policy_test
    meeting_callback_gate_state_test
    meeting_send_queue_adapter_test
    meeting_app_failure_policy_test
    meeting_outcome_replay_policy_test
    meeting_callback_shutdown_test
)
    add_executable(${policy_test} ${policy_test}.cpp)
    target_include_directories(${policy_test} PRIVATE
        ../main/stackchan
        ../main/apps/app_launcher/view
    )
    target_compile_features(${policy_test} PRIVATE cxx_std_17)
    add_test(NAME ${policy_test} COMMAND $<TARGET_FILE:${policy_test}>)
endforeach()
