package web_socket

import (
	"errors"
	"net/http/httptest"
	"stackChan/internal/meetingsecurity"
	"testing"
)

func TestAuthorizeUpgradeEnforcesRequestPolicy(t *testing.T) {
	previousPolicy, previousLimiter := meetingRequestPolicy, meetingUpgradeLimiter
	t.Cleanup(func() {
		meetingRequestPolicy, meetingUpgradeLimiter = previousPolicy, previousLimiter
	})
	meetingRequestPolicy = meetingsecurity.RequestPolicy{AllowedHosts: []string{"stackchan.example.com"}}
	meetingUpgradeLimiter = meetingsecurity.NewFixedWindowLimiter(10, 60, nil)
	request := httptest.NewRequest("GET", "https://attacker.example/stackChan/ws", nil)

	if err := authorizeMeetingUpgrade(request); !errors.Is(err, meetingsecurity.ErrHostNotAllowed) {
		t.Fatalf("authorizeMeetingUpgrade() error = %v, want host rejection", err)
	}
}

func TestAuthorizeUpgradeRateLimitsByRemoteAddress(t *testing.T) {
	previousPolicy, previousLimiter := meetingRequestPolicy, meetingUpgradeLimiter
	t.Cleanup(func() {
		meetingRequestPolicy, meetingUpgradeLimiter = previousPolicy, previousLimiter
	})
	meetingRequestPolicy = meetingsecurity.RequestPolicy{AllowedHosts: []string{"stackchan.example.com"}}
	meetingUpgradeLimiter = meetingsecurity.NewFixedWindowLimiter(1, 60, nil)
	request := httptest.NewRequest("GET", "https://stackchan.example.com/stackChan/ws", nil)
	request.RemoteAddr = "192.0.2.1:1234"

	if err := authorizeMeetingUpgrade(request); err != nil {
		t.Fatalf("first upgrade was rejected: %v", err)
	}
	if err := authorizeMeetingUpgrade(request); !errors.Is(err, ErrUpgradeRateLimited) {
		t.Fatalf("second upgrade error = %v, want rate limit", err)
	}
}
