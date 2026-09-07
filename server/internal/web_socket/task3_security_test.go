package web_socket

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"stackChan/internal/model"
	"stackChan/internal/pairing"
)

func testWebSocketConn() *websocket.Conn { return &websocket.Conn{} }

func TestMeetingAuthorizationRequiresTicketMACBindingDeviceAndGeneration(t *testing.T) {
	repository := pairing.NewMemoryRepository(time.Now)
	mac := "AABBCCDDEEFF"
	nonce, _ := repository.IssueNonce(mac, 1)
	if err := repository.Bind("owner", mac, nonce.Value, 1); err != nil {
		t.Fatal(err)
	}
	client := model.NewAppClient(mac, nil, "phone")
	defer client.CloseWriterCoroutine()
	client.SetMeetingAuthorization("owner", "phone", 3)
	if err := authorizeAppMeetingCommand(repository, client, 3, mac); err != nil {
		t.Fatalf("authorized command rejected: %v", err)
	}
	if err := authorizeAppMeetingCommand(repository, client, 3, "112233445566"); err == nil {
		t.Fatal("ticket(A)+command(B) was authorized")
	}
	repository.Unbind("owner", mac)
	if err := authorizeAppMeetingCommand(repository, client, 3, mac); err == nil {
		t.Fatal("open connection remained authorized after unbind")
	}
	if err := authorizeAppMeetingCommand(repository, client, 2, mac); err == nil {
		t.Fatal("old attachment generation was authorized")
	}
}

func TestMeetingSelectionRequiresBothCurrentConnectionsAndResetsOnReconnect(t *testing.T) {
	mac := "AABBCCDDEEFF"
	app := model.NewAppClient(mac, nil, "phone")
	device := model.NewStackChanClient(mac, nil, nil, nil, false)
	defer app.CloseWriterCoroutine()
	defer device.CloseWriterCoroutine()
	appGeneration := app.ReplaceConnection(testWebSocketConn())
	deviceGeneration := device.ReplaceConnection(testWebSocketConn())
	app.SetMeetingAuthorization("owner", "phone", appGeneration)
	app.SelectMeetingV1(appGeneration)
	if bothMeetingV1Selected(app, appGeneration, device, deviceGeneration) {
		t.Fatal("one-sided selection enabled meeting")
	}
	device.SelectMeetingV1(deviceGeneration)
	if !bothMeetingV1Selected(app, appGeneration, device, deviceGeneration) {
		t.Fatal("both selected connections were rejected")
	}
	newGeneration := device.ReplaceConnection(testWebSocketConn())
	if bothMeetingV1Selected(app, appGeneration, device, newGeneration) {
		t.Fatal("reconnect retained selected capability")
	}
}

func TestLegacyAppCannotObserveMeetingOnlineState(t *testing.T) {
	legacy := model.NewAppClient("AABBCCDDEEFF", nil, "phone")
	defer legacy.CloseWriterCoroutine()
	if canObserveMeetingState(legacy, legacy.ConnectionGeneration()) {
		t.Fatal("legacy app can infer target online state")
	}
}

func TestConnectionRateLimiterBoundsMessagesBytesControlAndAudio(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newConnectionRateLimiter(func() time.Time { return now }, connectionRateLimits{Messages: 3, Bytes: 12, Control: 1, Audio: 1, Window: time.Second})
	if !limiter.Allow(MeetingControl, 5) || limiter.Allow(MeetingControl, 1) {
		t.Fatal("control rate limit not enforced")
	}
	if !limiter.Allow(Opus, 5) || limiter.Allow(Opus, 1) {
		t.Fatal("audio rate limit not enforced")
	}
	if limiter.Allow(TextMessage, 3) {
		t.Fatal("message count limit not enforced")
	}
	now = now.Add(time.Second)
	if !limiter.Allow(TextMessage, 12) || limiter.Allow(TextMessage, 1) {
		t.Fatal("byte limit/reset not enforced")
	}
}
