#ifndef STACKCHAN_MEETING_CALLBACK_SHUTDOWN_H
#define STACKCHAN_MEETING_CALLBACK_SHUTDOWN_H

// Production teardown ordering seam: entry is disabled first, all callback
// bodies that already entered are drained second, and socket ownership is
// released only after the drain barrier has completed.
template <typename DisableEntry, typename DrainCallbacks, typename ResetSocket>
void ShutdownMeetingCallbacks(
    DisableEntry&& disable_entry,
    DrainCallbacks&& drain_callbacks,
    ResetSocket&& reset_socket
) {
    disable_entry();
    drain_callbacks();
    reset_socket();
}

#endif
