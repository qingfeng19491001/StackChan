package cmd

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestMeetingRedisIsOptional(t *testing.T) {
	t.Setenv("STACKCHAN_REDIS_ADDR", "")
	closeStore, err := configureMeetingSessionStore(context.Background())
	if err != nil {
		t.Fatalf("memory-only configuration: %v", err)
	}
	closeStore()
}

func TestConfiguredMeetingRedisMustBeReachable(t *testing.T) {
	server := miniredis.RunT(t)
	t.Setenv("STACKCHAN_REDIS_ADDR", server.Addr())
	t.Setenv("STACKCHAN_REDIS_NAMESPACE", "runtime-test")
	t.Setenv("STACKCHAN_MEETING_SESSION_TTL_SECONDS", "60")
	closeStore, err := configureMeetingSessionStore(context.Background())
	if err != nil {
		t.Fatalf("Redis configuration: %v", err)
	}
	closeStore()
}

func TestConfiguredMeetingRedisRejectsInvalidDatabase(t *testing.T) {
	t.Setenv("STACKCHAN_REDIS_ADDR", "127.0.0.1:6379")
	t.Setenv("STACKCHAN_REDIS_DB", "-1")
	if _, err := configureMeetingSessionStore(context.Background()); err == nil {
		t.Fatal("negative Redis database was accepted")
	}
}
