#include "meeting_callback_shutdown.h"

extern "C" int run_tests()
{
    bool entry_enabled = true;
    bool callback_in_flight = true;
    bool socket_owned = true;
    bool reset_before_drain = false;
    unsigned int phase = 0;

    ShutdownMeetingCallbacks(
        [&]() {
            if (phase != 0) phase = 99;
            entry_enabled = false;
            phase = 1;
        },
        [&]() {
            if (phase != 1 || entry_enabled) phase = 99;
            callback_in_flight = false;
            phase = 2;
        },
        [&]() {
            if (phase != 2 || callback_in_flight) reset_before_drain = true;
            socket_owned = false;
            phase = 3;
        }
    );
    if (phase != 3 || entry_enabled || callback_in_flight || socket_owned || reset_before_drain) {
        return __LINE__;
    }
    return 0;
}

#ifndef STACKCHAN_WASM_TEST
int main() { return run_tests(); }
#endif
