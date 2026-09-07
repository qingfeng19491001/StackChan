package pairing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gogf/gf/v2/frame/g"
	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/gogf/gf/v2/util/guid"
	"stackChan/internal/meetingsecurity"
)

func TestPairingHTTPFlowIssuesSingleUseTicketAndUnbinds(t *testing.T) {
	repository := NewMemoryRepository(time.Now)
	policy := meetingsecurity.RequestPolicy{AllowedHosts: []string{"127.0.0.1"}}
	handlers := HTTPHandlers{
		Repository: repository,
		AuthenticateDevice: func(*ghttp.Request) (string, error) {
			return "AABBCCDDEEFF", nil
		},
		AuthenticateUser: func(*ghttp.Request) (string, error) {
			return "user-1", nil
		},
		DeviceGeneration: func(string) (uint64, bool) { return 7, true },
		RequestPolicy:    &policy,
		RateLimiter:      meetingsecurity.NewFixedWindowLimiter(20, 60, nil),
	}
	server := g.Server(guid.S())
	server.SetAddr("127.0.0.1:0")
	server.SetDumpRouterMap(false)
	server.BindHandler("POST:/stackChan/pairing-nonce", handlers.PairingNonce)
	server.BindHandler("POST:/stackChan/bind", handlers.Bind)
	server.BindHandler("GET:/stackChan/devices", handlers.Devices)
	server.BindHandler("POST:/stackChan/ws-ticket", handlers.WSTicket)
	server.BindHandler("POST:/stackChan/unbind", handlers.Unbind)
	server.BindHandler("DELETE:/stackChan/bind/:mac", handlers.UnbindPath)
	if err := server.Start(); err != nil {
		t.Fatalf("start test server: %v", err)
	}
	t.Cleanup(func() { _ = server.Shutdown() })
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", server.GetListenedPort())

	nonceResponse := requestJSON(t, http.MethodPost, baseURL+"/stackChan/pairing-nonce", map[string]any{"mac": "AABBCCDDEEFF", "protocolVersion": 1}, http.StatusOK)
	pairURI, _ := nonceResponse["pairUri"].(string)
	if pairURI == "" {
		t.Fatalf("pairing response omitted pairUri: %#v", nonceResponse)
	}
	parsedNonce := parseNonceFromPairURI(t, pairURI)

	requestJSON(t, http.MethodPost, baseURL+"/stackChan/bind", map[string]any{"mac": "AABBCCDDEEFF", "nonce": parsedNonce}, http.StatusOK)
	devices := requestJSON(t, http.MethodGet, baseURL+"/stackChan/devices", nil, http.StatusOK)
	deviceList, ok := devices["devices"].([]any)
	if !ok || len(deviceList) != 1 {
		t.Fatalf("devices response = %#v, want one device", devices)
	}

	ticketResponse := requestJSON(t, http.MethodPost, baseURL+"/stackChan/ws-ticket", map[string]any{"mac": "AABBCCDDEEFF", "deviceID": "installation-1"}, http.StatusOK)
	ticket, _ := ticketResponse["ticket"].(string)
	claims, err := repository.ConsumeTicket(ticket)
	if err != nil || claims.UserID != "user-1" || claims.DeviceID != "installation-1" {
		t.Fatalf("ticket claims = %#v, error = %v", claims, err)
	}
	if _, err := repository.ConsumeTicket(ticket); err != ErrTicketInvalid {
		t.Fatalf("second ticket consume error = %v, want %v", err, ErrTicketInvalid)
	}

	requestJSON(t, http.MethodDelete, baseURL+"/stackChan/bind/AABBCCDDEEFF", nil, http.StatusOK)
	if _, bound := repository.OwnerOf("AABBCCDDEEFF"); bound {
		t.Fatal("device remained bound after unbind")
	}
}

func parseNonceFromPairURI(t *testing.T, pairURI string) string {
	t.Helper()
	const marker = "nonce="
	start := bytes.Index([]byte(pairURI), []byte(marker))
	if start < 0 {
		t.Fatalf("pair URI has no nonce: %s", pairURI)
	}
	value := pairURI[start+len(marker):]
	if end := bytes.IndexByte([]byte(value), '&'); end >= 0 {
		value = value[:end]
	}
	return value
}

func requestJSON(t *testing.T, method, url string, body any, wantStatus int) map[string]any {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response from %s: %v", url, err)
	}
	if response.StatusCode != wantStatus {
		t.Fatalf("%s status = %d, want %d; body = %#v", url, response.StatusCode, wantStatus, decoded)
	}
	return decoded
}
