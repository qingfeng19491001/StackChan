package pairing

import (
	"errors"
	"testing"
	"time"
)

func TestNonceBindsOnceToOnlineConnectionGeneration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	repository := NewMemoryRepository(func() time.Time { return now })
	nonce, err := repository.IssueNonce("aa:bb:cc:dd:ee:ff", 7)
	if err != nil {
		t.Fatal(err)
	}
	if nonce.MAC != "AABBCCDDEEFF" || nonce.ExpiresAt != now.Add(2*time.Minute).Unix() {
		t.Fatalf("unexpected nonce: %+v", nonce)
	}

	if err := repository.Bind("user-1", nonce.MAC, nonce.Value, 8); !errors.Is(err, ErrConnectionChanged) {
		t.Fatalf("generation mismatch = %v", err)
	}
	if err := repository.Bind("user-1", nonce.MAC, nonce.Value, 7); err != nil {
		t.Fatal(err)
	}
	if err := repository.Bind("user-1", nonce.MAC, nonce.Value, 7); !errors.Is(err, ErrNonceInvalid) {
		t.Fatalf("nonce reuse = %v", err)
	}
	devices := repository.Devices("user-1")
	if len(devices) != 1 || devices[0].MAC != nonce.MAC {
		t.Fatalf("devices = %+v", devices)
	}
}

func TestExpiredNonceCannotBind(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	repository := NewMemoryRepository(func() time.Time { return now })
	nonce, _ := repository.IssueNonce("AABBCCDDEEFF", 1)
	now = now.Add(121 * time.Second)
	if err := repository.Bind("user-1", nonce.MAC, nonce.Value, 1); !errors.Is(err, ErrNonceExpired) {
		t.Fatalf("expired nonce = %v", err)
	}
}

func TestTicketIsScopedShortLivedAndSingleUse(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	repository := NewMemoryRepository(func() time.Time { return now })
	nonce, _ := repository.IssueNonce("AABBCCDDEEFF", 1)
	if err := repository.Bind("user-1", nonce.MAC, nonce.Value, 1); err != nil {
		t.Fatal(err)
	}

	ticket, err := repository.IssueTicket("user-1", nonce.MAC, "app", "phone-1")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := repository.ConsumeTicket(ticket.Value)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != "user-1" || claims.MAC != nonce.MAC || claims.DeviceID != "phone-1" || claims.Role != "app" {
		t.Fatalf("claims = %+v", claims)
	}
	if _, err := repository.ConsumeTicket(ticket.Value); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("ticket reuse = %v", err)
	}

	second, _ := repository.IssueTicket("user-1", nonce.MAC, "app", "phone-1")
	now = now.Add(61 * time.Second)
	if _, err := repository.ConsumeTicket(second.Value); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("expired ticket = %v", err)
	}
}

func TestTicketRequiresOwnershipAndUnbindRevokesAccess(t *testing.T) {
	repository := NewMemoryRepository(time.Now)
	if _, err := repository.IssueTicket("user-1", "AABBCCDDEEFF", "app", "phone-1"); !errors.Is(err, ErrNotBound) {
		t.Fatalf("unbound ticket = %v", err)
	}
	nonce, _ := repository.IssueNonce("AABBCCDDEEFF", 1)
	_ = repository.Bind("user-1", nonce.MAC, nonce.Value, 1)
	if !repository.Unbind("user-1", nonce.MAC) {
		t.Fatal("unbind failed")
	}
	if _, err := repository.IssueTicket("user-1", nonce.MAC, "app", "phone-1"); !errors.Is(err, ErrNotBound) {
		t.Fatalf("ticket after unbind = %v", err)
	}
}

func TestOwnerOfReturnsOnlyCurrentBinding(t *testing.T) {
	repository := NewMemoryRepository(time.Now)
	nonce, _ := repository.IssueNonce("AA:BB:CC:DD:EE:FF", 3)
	_ = repository.Bind("user-1", nonce.MAC, nonce.Value, 3)

	owner, ok := repository.OwnerOf("aa-bb-cc-dd-ee-ff")
	if !ok || owner != "user-1" {
		t.Fatalf("owner=%q ok=%v", owner, ok)
	}
	repository.Unbind("user-1", nonce.MAC)
	if owner, ok := repository.OwnerOf(nonce.MAC); ok || owner != "" {
		t.Fatalf("owner after unbind=%q ok=%v", owner, ok)
	}
}
