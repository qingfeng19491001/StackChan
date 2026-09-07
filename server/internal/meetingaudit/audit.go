// Package meetingaudit records security-relevant meeting events without
// retaining raw audio, tickets, nonces, or device credentials.
package meetingaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"
)

type Event struct {
	Action    string
	Outcome   string
	MAC       string
	UserID    string
	SessionID string
	Code      string
}

var logger = slog.Default()

func Record(event Event) {
	if event.Action == "" {
		return
	}
	logger.Info("meeting audit",
		"action", event.Action,
		"outcome", emptyAs(event.Outcome, "accepted"),
		"mac", event.MAC,
		"user", redact(event.UserID),
		"session", redact(event.SessionID),
		"code", event.Code,
		"at", time.Now().UTC().Format(time.RFC3339),
	)
}

func redact(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func emptyAs(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
