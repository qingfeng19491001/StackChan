package assemblyai

import (
	"errors"
	"testing"
	"time"
)

func TestJobTokenRoundTripAcrossInstances(t *testing.T) {
	first := JobTokenSigner{Secret: []byte("32-byte-test-signing-secret-value")}
	second := JobTokenSigner{Secret: []byte("32-byte-test-signing-secret-value")}
	token, err := first.Sign("transcript-1", "user-1", time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.Verify(token, "user-1", time.Unix(1000, 0))
	if err != nil || got != "transcript-1" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestJobTokenRejectsWrongOwnerExpiryAndTampering(t *testing.T) {
	signer := JobTokenSigner{Secret: []byte("32-byte-test-signing-secret-value")}
	token, err := signer.Sign("transcript-1", "user-1", time.Unix(2000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signer.Verify(token, "user-2", time.Unix(1000, 0)); !errors.Is(err, ErrInvalidJobToken) {
		t.Fatalf("wrong owner error=%v", err)
	}
	if _, err := signer.Verify(token, "user-1", time.Unix(2001, 0)); !errors.Is(err, ErrExpiredJobToken) {
		t.Fatalf("expiry error=%v", err)
	}
	if _, err := signer.Verify(token+"x", "user-1", time.Unix(1000, 0)); !errors.Is(err, ErrInvalidJobToken) {
		t.Fatalf("tamper error=%v", err)
	}
}

func TestJobTokenRequiresStrongSecretAndClaims(t *testing.T) {
	if _, err := (JobTokenSigner{Secret: []byte("short")}).Sign("transcript-1", "user-1", time.Now()); err == nil {
		t.Fatal("expected short secret to fail")
	}
	signer := JobTokenSigner{Secret: []byte("32-byte-test-signing-secret-value")}
	if _, err := signer.Sign("", "user-1", time.Now()); err == nil {
		t.Fatal("expected empty transcript id to fail")
	}
	if _, err := signer.Sign("transcript-1", "", time.Now()); err == nil {
		t.Fatal("expected empty user id to fail")
	}
}
