package pairing

import (
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteRepositoryRestoresBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairing.db")
	now := time.Now
	r, err := NewSQLiteRepository(path, now)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := r.IssueNonce("AABBCCDDEEFF", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Bind("user-1", "AABBCCDDEEFF", nonce.Value, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := NewSQLiteRepository(path, now)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	devices := r2.Devices("user-1")
	if len(devices) != 1 || devices[0].MAC != "AABBCCDDEEFF" {
		t.Fatalf("restored devices = %#v", devices)
	}
	if owner, ok := r2.OwnerOf("AABBCCDDEEFF"); !ok || owner != "user-1" {
		t.Fatalf("restored owner = %q, %v", owner, ok)
	}
}
