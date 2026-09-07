package meetingsecurity

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestRequestPolicyAllowsNativeTLSRequestForConfiguredHost(t *testing.T) {
	policy := RequestPolicy{
		AllowedHosts: []string{"stackchan.example.com"},
		RequireTLS:   true,
	}
	request := httptest.NewRequest("GET", "https://stackchan.example.com/stackChan/ws", nil)
	request.TLS = &tls.ConnectionState{}

	if err := policy.Validate(request); err != nil {
		t.Fatalf("native TLS request was rejected: %v", err)
	}
}

func TestRequestPolicyRejectsUnconfiguredHost(t *testing.T) {
	policy := RequestPolicy{AllowedHosts: []string{"stackchan.example.com"}}
	request := httptest.NewRequest("GET", "https://attacker.example/stackChan/ws", nil)

	if err := policy.Validate(request); err != ErrHostNotAllowed {
		t.Fatalf("Validate() error = %v, want %v", err, ErrHostNotAllowed)
	}
}

func TestRequestPolicyRejectsPlainHTTPWhenTLSRequired(t *testing.T) {
	policy := RequestPolicy{AllowedHosts: []string{"stackchan.example.com"}, RequireTLS: true}
	request := httptest.NewRequest("GET", "http://stackchan.example.com/stackChan/ws", nil)

	if err := policy.Validate(request); err != ErrTLSRequired {
		t.Fatalf("Validate() error = %v, want %v", err, ErrTLSRequired)
	}
}

func TestRequestPolicyAllowsMissingOriginButChecksBrowserOrigin(t *testing.T) {
	policy := RequestPolicy{
		AllowedHosts:   []string{"stackchan.example.com"},
		AllowedOrigins: []string{"https://app.example.com"},
	}
	nativeRequest := httptest.NewRequest("GET", "https://stackchan.example.com/stackChan/ws", nil)
	if err := policy.Validate(nativeRequest); err != nil {
		t.Fatalf("request without Origin was rejected: %v", err)
	}
	browserRequest := httptest.NewRequest("GET", "https://stackchan.example.com/stackChan/ws", nil)
	browserRequest.Header.Set("Origin", "https://evil.example")
	if err := policy.Validate(browserRequest); err != ErrOriginNotAllowed {
		t.Fatalf("Validate() error = %v, want %v", err, ErrOriginNotAllowed)
	}
}

func TestFixedWindowLimiterRejectsOnlyAfterConfiguredBurst(t *testing.T) {
	now := int64(1_000)
	limiter := NewFixedWindowLimiter(2, 60, func() int64 { return now })
	if !limiter.Allow("user-1") || !limiter.Allow("user-1") {
		t.Fatal("limiter rejected a request inside the burst")
	}
	if limiter.Allow("user-1") {
		t.Fatal("limiter accepted a request beyond the burst")
	}
	now += 60
	if !limiter.Allow("user-1") {
		t.Fatal("limiter did not reset at the next window")
	}
}
