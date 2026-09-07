package meetingmetrics

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"sync"
	"time"
)

func AuthorizedHandler(registry *Registry, token string) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		provided := request.Header.Get("Authorization")
		expected := "Bearer " + token
		if token == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		registry.ServeHTTP(response, request)
	})
}

type Registry struct {
	mu                  sync.RWMutex
	onlineDevices       int64
	activeSessions      int64
	firstFrameCount     uint64
	firstFrameSeconds   float64
	framesTotal         uint64
	framesMissing       uint64
	reconnects          uint64
	sendQueueDepth      int64
	meetingsCompleted   uint64
	meetingsInterrupted uint64
}

var Default = NewRegistry()

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) SetOnlineDevices(value int64) {
	r.mu.Lock()
	r.onlineDevices = value
	r.mu.Unlock()
}

func (r *Registry) SetActiveSessions(value int64) {
	r.mu.Lock()
	r.activeSessions = value
	r.mu.Unlock()
}

func (r *Registry) ObserveFirstFrame(value time.Duration) {
	r.mu.Lock()
	r.firstFrameCount++
	r.firstFrameSeconds += value.Seconds()
	r.mu.Unlock()
}

func (r *Registry) AddFrames(received, missing uint64) {
	r.mu.Lock()
	r.framesTotal += received + missing
	r.framesMissing += missing
	r.mu.Unlock()
}

func (r *Registry) IncReconnect() {
	r.mu.Lock()
	r.reconnects++
	r.mu.Unlock()
}

func (r *Registry) SetSendQueueDepth(value int64) {
	r.mu.Lock()
	r.sendQueueDepth = value
	r.mu.Unlock()
}

func (r *Registry) IncCompleted() {
	r.mu.Lock()
	r.meetingsCompleted++
	r.mu.Unlock()
}

func (r *Registry) IncInterrupted() {
	r.mu.Lock()
	r.meetingsInterrupted++
	r.mu.Unlock()
}

func (r *Registry) ServeHTTP(response http.ResponseWriter, _ *http.Request) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(response, `# TYPE stackchan_online_devices gauge
stackchan_online_devices %d
# TYPE stackchan_active_meetings gauge
stackchan_active_meetings %d
# TYPE stackchan_meeting_first_frame_latency_seconds summary
stackchan_meeting_first_frame_latency_seconds_count %d
stackchan_meeting_first_frame_latency_seconds_sum %g
# TYPE stackchan_meeting_frames_total counter
stackchan_meeting_frames_total %d
# TYPE stackchan_meeting_frames_missing_total counter
stackchan_meeting_frames_missing_total %d
# TYPE stackchan_meeting_ws_reconnects_total counter
stackchan_meeting_ws_reconnects_total %d
# TYPE stackchan_meeting_send_queue_depth gauge
stackchan_meeting_send_queue_depth %d
# TYPE stackchan_meetings_completed_total counter
stackchan_meetings_completed_total %d
# TYPE stackchan_meetings_interrupted_total counter
stackchan_meetings_interrupted_total %d
`, r.onlineDevices, r.activeSessions, r.firstFrameCount, r.firstFrameSeconds,
		r.framesTotal, r.framesMissing, r.reconnects, r.sendQueueDepth,
		r.meetingsCompleted, r.meetingsInterrupted)
}
