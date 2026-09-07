package web_socket

import (
	"testing"
	"time"
)

func TestDeviceCredentialRotationRequiresAnUnexpiredGraceWindow(t *testing.T) {
	t.Setenv("STACKCHAN_DEVICE_CREDENTIALS", "AABBCCDDEEFF=current-secret")
	t.Setenv("STACKCHAN_DEVICE_CREDENTIALS_PREVIOUS", "AABBCCDDEEFF=previous-secret")
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	t.Setenv("STACKCHAN_DEVICE_CREDENTIALS_PREVIOUS_UNTIL", now.Add(time.Hour).Format(time.RFC3339))
	if mac, ok := deviceCredentialMAC("current-secret", now); !ok || mac != "AABBCCDDEEFF" {
		t.Fatalf("current credential = %q, %v", mac, ok)
	}
	if mac, ok := deviceCredentialMAC("previous-secret", now); !ok || mac != "AABBCCDDEEFF" {
		t.Fatalf("previous credential in grace = %q, %v", mac, ok)
	}
	if _, ok := deviceCredentialMAC("previous-secret", now.Add(2*time.Hour)); ok {
		t.Fatal("previous credential survived expired grace window")
	}
}
