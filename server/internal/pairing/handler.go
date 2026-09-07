package pairing

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"stackChan/internal/meetingaudit"
	"stackChan/internal/meetingsecurity"
	"strings"

	"github.com/gogf/gf/v2/net/ghttp"
)

type DeviceAuthenticator func(*ghttp.Request) (string, error)
type UserAuthenticator func(*ghttp.Request) (string, error)
type DeviceGenerationLookup func(string) (uint64, bool)

type HTTPHandlers struct {
	Repository         Repository
	AuthenticateDevice DeviceAuthenticator
	AuthenticateUser   UserAuthenticator
	DeviceGeneration   DeviceGenerationLookup
	RequestPolicy      *meetingsecurity.RequestPolicy
	RateLimiter        *meetingsecurity.FixedWindowLimiter
}

var ErrRateLimited = errors.New("pairing request rate limited")

func (h HTTPHandlers) authorizeRequest(request *http.Request) error {
	if h.RequestPolicy != nil {
		if err := h.RequestPolicy.Validate(request); err != nil {
			return err
		}
	}
	key := request.RemoteAddr
	if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		key = host
	}
	if h.RateLimiter != nil && !h.RateLimiter.Allow(key) {
		return ErrRateLimited
	}
	return nil
}

func (h HTTPHandlers) guard(r *ghttp.Request) bool {
	if err := h.authorizeRequest(r.Request); err != nil {
		status := http.StatusForbidden
		code := "REQUEST_REJECTED"
		if errors.Is(err, ErrRateLimited) {
			status, code = http.StatusTooManyRequests, "RATE_LIMITED"
		}
		h.write(r, status, map[string]string{"code": code})
		return false
	}
	return true
}

func (h HTTPHandlers) write(r *ghttp.Request, status int, value any) {
	r.Response.WriteHeader(status)
	r.Response.Header().Set("Content-Type", "application/json")
	r.Response.WriteJson(value)
}

func decodeRequest(r *ghttp.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(r.Response.Writer, r.Request.Body, 16*1024))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func (h HTTPHandlers) PairingNonce(r *ghttp.Request) {
	if !h.guard(r) {
		return
	}
	mac, err := h.AuthenticateDevice(r)
	if err != nil {
		h.write(r, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})
		return
	}
	var request struct {
		MAC             string `json:"mac"`
		ProtocolVersion int    `json:"protocolVersion"`
	}
	if decodeRequest(r, &request) != nil || request.ProtocolVersion != 1 {
		h.write(r, http.StatusBadRequest, map[string]string{"code": "PROTOCOL_UNSUPPORTED"})
		return
	}
	requestMAC, err := NormalizeMAC(request.MAC)
	if err != nil || requestMAC != mac {
		h.write(r, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})
		return
	}
	generation, online := h.DeviceGeneration(mac)
	if !online {
		h.write(r, http.StatusConflict, map[string]string{"code": "DEVICE_OFFLINE"})
		return
	}
	nonce, err := h.Repository.IssueNonce(mac, generation)
	if err != nil {
		h.write(r, http.StatusInternalServerError, map[string]string{"code": "DEVICE_ERROR"})
		return
	}
	pairURI := fmt.Sprintf("stackchan://pair?v=1&mac=%s&nonce=%s&exp=%d", mac, url.QueryEscape(nonce.Value), nonce.ExpiresAt)
	h.write(r, http.StatusOK, map[string]any{"pairUri": pairURI, "expiresAt": nonce.ExpiresAt})
}

func (h HTTPHandlers) Bind(r *ghttp.Request) {
	if !h.guard(r) {
		return
	}
	userID, err := h.AuthenticateUser(r)
	if err != nil {
		h.write(r, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})
		return
	}
	var request struct{ MAC, Nonce string }
	if decodeRequest(r, &request) != nil {
		h.write(r, http.StatusBadRequest, map[string]string{"code": "INVALID_REQUEST"})
		return
	}
	mac, err := NormalizeMAC(request.MAC)
	if err != nil {
		h.write(r, http.StatusBadRequest, map[string]string{"code": "INVALID_REQUEST"})
		return
	}
	generation, online := h.DeviceGeneration(mac)
	if !online {
		h.write(r, http.StatusConflict, map[string]string{"code": "DEVICE_OFFLINE"})
		return
	}
	if err := h.Repository.Bind(userID, mac, request.Nonce, generation); err != nil {
		meetingaudit.Record(meetingaudit.Event{Action: "pair.bind", Outcome: "rejected", MAC: mac, UserID: userID, Code: err.Error()})
		h.write(r, http.StatusConflict, map[string]string{"code": "NOT_BOUND", "message": err.Error()})
		return
	}
	meetingaudit.Record(meetingaudit.Event{Action: "pair.bind", MAC: mac, UserID: userID})
	h.write(r, http.StatusOK, map[string]any{"mac": mac, "bound": true})
}

func (h HTTPHandlers) Devices(r *ghttp.Request) {
	if !h.guard(r) {
		return
	}
	userID, err := h.AuthenticateUser(r)
	if err != nil {
		h.write(r, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})
		return
	}
	devices := h.Repository.Devices(userID)
	result := make([]map[string]any, 0, len(devices))
	for _, device := range devices {
		_, online := h.DeviceGeneration(device.MAC)
		result = append(result, map[string]any{"mac": device.MAC, "boundAt": device.BoundAt, "online": online})
	}
	h.write(r, http.StatusOK, map[string]any{"devices": result})
}

func (h HTTPHandlers) Unbind(r *ghttp.Request) {
	if !h.guard(r) {
		return
	}
	userID, err := h.AuthenticateUser(r)
	if err != nil {
		h.write(r, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})
		return
	}
	var request struct{ MAC string }
	if decodeRequest(r, &request) != nil {
		h.write(r, http.StatusBadRequest, map[string]string{"code": "INVALID_REQUEST"})
		return
	}
	h.unbind(r, userID, request.MAC)
}

// UnbindPath is the canonical meeting-v1 endpoint. Unbind above remains as a
// compatibility route for already released clients.
func (h HTTPHandlers) UnbindPath(r *ghttp.Request) {
	if !h.guard(r) {
		return
	}
	userID, err := h.AuthenticateUser(r)
	if err != nil {
		h.write(r, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})
		return
	}
	h.unbind(r, userID, r.Get("mac").String())
}

func (h HTTPHandlers) unbind(r *ghttp.Request, userID, mac string) {
	normalized, err := NormalizeMAC(mac)
	if err != nil {
		h.write(r, http.StatusBadRequest, map[string]string{"code": "INVALID_REQUEST"})
		return
	}
	if !h.Repository.Unbind(userID, normalized) {
		meetingaudit.Record(meetingaudit.Event{Action: "pair.unbind", Outcome: "rejected", MAC: normalized, UserID: userID, Code: "NOT_BOUND"})
		h.write(r, http.StatusNotFound, map[string]string{"code": "NOT_BOUND"})
		return
	}
	meetingaudit.Record(meetingaudit.Event{Action: "pair.unbind", MAC: normalized, UserID: userID})
	h.write(r, http.StatusOK, map[string]any{"unbound": true})
}

func (h HTTPHandlers) WSTicket(r *ghttp.Request) {
	if !h.guard(r) {
		return
	}
	userID, err := h.AuthenticateUser(r)
	if err != nil {
		h.write(r, http.StatusUnauthorized, map[string]string{"code": "UNAUTHORIZED"})
		return
	}
	var request struct{ MAC, DeviceID string }
	if decodeRequest(r, &request) != nil {
		h.write(r, http.StatusBadRequest, map[string]string{"code": "INVALID_REQUEST"})
		return
	}
	if _, online := h.DeviceGeneration(request.MAC); !online {
		meetingaudit.Record(meetingaudit.Event{Action: "pair.ticket", Outcome: "rejected", MAC: request.MAC, UserID: userID, Code: "DEVICE_OFFLINE"})
		h.write(r, http.StatusConflict, map[string]string{"code": "DEVICE_OFFLINE"})
		return
	}
	ticket, err := h.Repository.IssueTicket(userID, request.MAC, "app", request.DeviceID)
	if err != nil {
		meetingaudit.Record(meetingaudit.Event{Action: "pair.ticket", Outcome: "rejected", MAC: request.MAC, UserID: userID, Code: "NOT_BOUND"})
		h.write(r, http.StatusForbidden, map[string]string{"code": "NOT_BOUND"})
		return
	}
	meetingaudit.Record(meetingaudit.Event{Action: "pair.ticket", MAC: request.MAC, UserID: userID})
	h.write(r, http.StatusOK, map[string]any{"ticket": ticket.Value, "expiresAt": ticket.ExpiresAt})
}

// LocalUserAuthenticator is deliberately disabled unless an explicit local
// user id is configured. Production deployments must replace it with JWKS auth.
func LocalUserAuthenticator(r *ghttp.Request) (string, error) {
	userID := strings.TrimSpace(os.Getenv("STACKCHAN_LOCAL_USER_ID"))
	if userID == "" || r.Header.Get("Authorization") != "Bearer local-dev" {
		return "", errors.New("local authentication disabled")
	}
	return userID, nil
}
