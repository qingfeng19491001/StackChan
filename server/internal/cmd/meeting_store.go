package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"stackChan/internal/meeting"
	"stackChan/internal/web_socket"
)

const defaultMeetingSessionTTL = 2 * time.Hour

func configureMeetingSessionStore(ctx context.Context) (func(), error) {
	enableReattach, err := optionalBoolean("STACKCHAN_MEETING_ENABLE_REATTACH", true)
	if err != nil {
		return nil, err
	}
	options := meeting.ManagerOptions{EnableReattach: enableReattach}
	address := strings.TrimSpace(os.Getenv("STACKCHAN_REDIS_ADDR"))
	if address == "" {
		web_socket.ConfigureMeetingRuntime(nil, options)
		return func() {}, nil
	}

	database, err := optionalPositiveInteger("STACKCHAN_REDIS_DB", 0, true)
	if err != nil {
		return nil, err
	}
	ttlSeconds, err := optionalPositiveInteger("STACKCHAN_MEETING_SESSION_TTL_SECONDS", int(defaultMeetingSessionTTL/time.Second), false)
	if err != nil {
		return nil, err
	}
	addresses := splitNonEmpty(address)
	if len(addresses) == 0 {
		return nil, fmt.Errorf("STACKCHAN_REDIS_ADDR contains no address")
	}
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs:    addresses,
		Username: os.Getenv("STACKCHAN_REDIS_USERNAME"),
		Password: os.Getenv("STACKCHAN_REDIS_PASSWORD"),
		DB:       database,
	})
	pingContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	store := meeting.NewRedisSessionStore(client, strings.TrimSpace(os.Getenv("STACKCHAN_REDIS_NAMESPACE")), time.Duration(ttlSeconds)*time.Second)
	if err := store.Ping(pingContext); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect to configured meeting Redis: %w", err)
	}
	web_socket.ConfigureMeetingRuntime(store, options)
	return func() { _ = client.Close() }, nil
}

func optionalBoolean(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return value, nil
}

func splitNonEmpty(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func optionalPositiveInteger(name string, fallback int, allowZero bool) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || (!allowZero && value == 0) {
		return 0, fmt.Errorf("%s must be %s integer", name, map[bool]string{true: "a non-negative", false: "a positive"}[allowZero])
	}
	return value, nil
}
