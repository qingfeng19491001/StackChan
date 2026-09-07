package pairing

import (
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisRepositorySharesBindingAndConsumesTicketOnce(t *testing.T) {
	server := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })
	first := NewRedisRepository(clientA, "test")
	second := NewRedisRepository(clientB, "test")
	mac := "AABBCCDDEEFF"
	nonce, err := first.IssueNonce(mac, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Bind("user-a", mac, nonce.Value, 7); err != nil {
		t.Fatalf("bind across instances: %v", err)
	}
	if owner, ok := first.OwnerOf(mac); !ok || owner != "user-a" {
		t.Fatalf("cross-instance owner = %q, %v", owner, ok)
	}
	if devices := first.Devices("user-a"); len(devices) != 1 || devices[0].MAC != mac {
		t.Fatalf("devices = %#v", devices)
	}
	ticket, err := first.IssueTicket("user-a", mac, "app", "phone-a")
	if err != nil {
		t.Fatal(err)
	}
	claims, err := second.ConsumeTicket(ticket.Value)
	if err != nil || claims.UserID != "user-a" || claims.DeviceID != "phone-a" {
		t.Fatalf("claims=%#v err=%v", claims, err)
	}
	if _, err := first.ConsumeTicket(ticket.Value); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("reused ticket error = %v", err)
	}
}

func TestRedisRepositoryRejectsStaleGenerationAndUnbindsEveryInstance(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	repository := NewRedisRepository(client, "test")
	nonce, err := repository.IssueNonce("AABBCCDDEEFF", 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Bind("user-a", "AABBCCDDEEFF", nonce.Value, 3); !errors.Is(err, ErrConnectionChanged) {
		t.Fatalf("stale generation = %v", err)
	}
	if err := repository.Bind("user-a", "AABBCCDDEEFF", nonce.Value, 2); err != nil {
		t.Fatal(err)
	}
	if !repository.Unbind("user-a", "AABBCCDDEEFF") {
		t.Fatal("unbind failed")
	}
	if _, bound := repository.OwnerOf("AABBCCDDEEFF"); bound {
		t.Fatal("binding remained after unbind")
	}
}

func TestRedisRepositoryUsesExpiryForNonces(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	now := time.Unix(1_700_000_000, 0)
	repository := NewRedisRepository(client, "test")
	repository.now = func() time.Time { return now }
	nonce, err := repository.IssueNonce("AABBCCDDEEFF", 1)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(NonceTTL + time.Second)
	if err := repository.Bind("user-a", "AABBCCDDEEFF", nonce.Value, 1); !errors.Is(err, ErrNonceExpired) {
		t.Fatalf("expired nonce = %v", err)
	}
}
