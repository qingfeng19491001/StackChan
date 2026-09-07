package web_socket

import (
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"stackChan/internal/meetingsecurity"
)

var ErrUpgradeRateLimited = errors.New("websocket upgrade rate limited")

var (
	meetingRequestPolicy  = requestPolicyFromEnvironment()
	meetingUpgradeLimiter = meetingsecurity.NewFixedWindowLimiter(integerEnvironment("STACKCHAN_WS_RATE_BURST", 30), 60, nil)
)

func MeetingRequestPolicy() *meetingsecurity.RequestPolicy {
	return &meetingRequestPolicy
}

func NewPairingRateLimiter() *meetingsecurity.FixedWindowLimiter {
	return meetingsecurity.NewFixedWindowLimiter(integerEnvironment("STACKCHAN_PAIRING_RATE_BURST", 60), 60, nil)
}

func requestPolicyFromEnvironment() meetingsecurity.RequestPolicy {
	hosts := csvEnvironment("STACKCHAN_ALLOWED_HOSTS")
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1"}
	}
	return meetingsecurity.RequestPolicy{
		AllowedHosts:   hosts,
		AllowedOrigins: csvEnvironment("STACKCHAN_ALLOWED_ORIGINS"),
		RequireTLS:     booleanEnvironment("STACKCHAN_REQUIRE_TLS", false),
	}
}

func csvEnvironment(name string) []string {
	var values []string
	for _, value := range strings.Split(os.Getenv(name), ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func integerEnvironment(name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func booleanEnvironment(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func authorizeMeetingUpgrade(request *http.Request) error {
	if err := meetingRequestPolicy.Validate(request); err != nil {
		return err
	}
	key := request.RemoteAddr
	if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		key = host
	}
	if !meetingUpgradeLimiter.Allow(key) {
		return ErrUpgradeRateLimited
	}
	return nil
}
