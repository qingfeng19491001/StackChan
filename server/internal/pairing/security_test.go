package pairing

import (
	"errors"
	"net/http/httptest"
	"stackChan/internal/meetingsecurity"
	"testing"
)

func TestHTTPHandlersAuthorizeRequestEnforcesHostPolicy(t *testing.T) {
	policy := meetingsecurity.RequestPolicy{AllowedHosts: []string{"stackchan.example.com"}}
	handlers := HTTPHandlers{RequestPolicy: &policy, RateLimiter: meetingsecurity.NewFixedWindowLimiter(10, 60, nil)}
	request := httptest.NewRequest("POST", "https://evil.example/stackChan/bind", nil)

	if err := handlers.authorizeRequest(request); !errors.Is(err, meetingsecurity.ErrHostNotAllowed) {
		t.Fatalf("authorizeRequest() error = %v, want host rejection", err)
	}
}

func TestHTTPHandlersAuthorizeRequestRateLimitsByRemoteAddress(t *testing.T) {
	policy := meetingsecurity.RequestPolicy{AllowedHosts: []string{"stackchan.example.com"}}
	handlers := HTTPHandlers{RequestPolicy: &policy, RateLimiter: meetingsecurity.NewFixedWindowLimiter(1, 60, nil)}
	request := httptest.NewRequest("POST", "https://stackchan.example.com/stackChan/bind", nil)
	request.RemoteAddr = "192.0.2.2:5678"

	if err := handlers.authorizeRequest(request); err != nil {
		t.Fatalf("first request rejected: %v", err)
	}
	if err := handlers.authorizeRequest(request); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("second request error = %v, want rate limit", err)
	}
}
