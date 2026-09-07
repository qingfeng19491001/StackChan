package web_socket

import (
	"crypto/subtle"
	"os"
	"strings"
	"time"

	"stackChan/internal/pairing"
)

// deviceCredentialMAC supports overlapping credential sets during a rollout.
// Current credentials are always accepted. Previous credentials require an
// explicit RFC3339 grace deadline, so a forgotten rotation cannot leave an
// old key valid indefinitely.
func deviceCredentialMAC(provided string, now time.Time) (string, bool) {
	if mac, ok := matchDeviceCredentials(provided, os.Getenv("STACKCHAN_DEVICE_CREDENTIALS")); ok {
		return mac, true
	}
	grace, err := time.Parse(time.RFC3339, strings.TrimSpace(os.Getenv("STACKCHAN_DEVICE_CREDENTIALS_PREVIOUS_UNTIL")))
	if err != nil || !now.Before(grace) {
		return "", false
	}
	return matchDeviceCredentials(provided, os.Getenv("STACKCHAN_DEVICE_CREDENTIALS_PREVIOUS"))
}

func matchDeviceCredentials(provided, values string) (string, bool) {
	for _, entry := range strings.Split(values, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
		if len(parts) != 2 || parts[1] == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(strings.TrimSpace(parts[1]))) != 1 {
			continue
		}
		mac, err := pairing.NormalizeMAC(parts[0])
		if err == nil {
			return mac, true
		}
	}
	return "", false
}
