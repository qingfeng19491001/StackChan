package meetingmetrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryExportsMeetingMetricsWithoutSessionLabels(t *testing.T) {
	registry := NewRegistry()
	registry.SetOnlineDevices(2)
	registry.SetActiveSessions(1)
	registry.ObserveFirstFrame(125 * time.Millisecond)
	registry.AddFrames(99, 1)
	registry.IncReconnect()
	registry.SetSendQueueDepth(4)
	registry.IncCompleted()
	registry.IncInterrupted()

	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, expected := range []string{
		"stackchan_online_devices 2",
		"stackchan_active_meetings 1",
		"stackchan_meeting_first_frame_latency_seconds_sum 0.125",
		"stackchan_meeting_frames_total 100",
		"stackchan_meeting_frames_missing_total 1",
		"stackchan_meeting_ws_reconnects_total 1",
		"stackchan_meeting_send_queue_depth 4",
		"stackchan_meetings_completed_total 1",
		"stackchan_meetings_interrupted_total 1",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("metrics output omitted %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "sessionId") || strings.Contains(body, "ticket") {
		t.Fatalf("metrics output contains sensitive identifiers: %s", body)
	}
}

func TestAuthorizedHandlerRejectsMissingToken(t *testing.T) {
	registry := NewRegistry()
	handler := AuthorizedHandler(registry, "metrics-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	if recorder.Code != 401 {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}

	request := httptest.NewRequest("GET", "/metrics", nil)
	request.Header.Set("Authorization", "Bearer metrics-secret")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "stackchan_online_devices") {
		t.Fatalf("authorized metrics response = %d %q", recorder.Code, recorder.Body.String())
	}
}
