/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package protocol

import (
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestParseAuthorizationClaimsRejectsMissingFieldsWithoutPanic(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, token := range []string{"", "mac", "mac|unused", "|unused|1000", "mac|unused|bad"} {
		if _, err := ParseAuthorizationClaims(token, now); !errors.Is(err, ErrInvalidAuthorizationToken) {
			t.Fatalf("token %q returned %v, want invalid token", token, err)
		}
	}
}

func TestParseAuthorizationClaimsChecksTimestampWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	if _, err := ParseAuthorizationClaims("mac|unused|989", now); !errors.Is(err, ErrAuthorizationTokenExpired) {
		t.Fatalf("old token returned %v", err)
	}
	if _, err := ParseAuthorizationClaims("mac|unused|1011", now); !errors.Is(err, ErrAuthorizationTokenExpired) {
		t.Fatalf("future token returned %v", err)
	}
	valid := "mac|unused|" + strconv.FormatInt(now.Unix(), 10)
	if mac, err := ParseAuthorizationClaims(valid, now); err != nil || mac != "mac" {
		t.Fatalf("valid token returned mac=%q err=%v", mac, err)
	}
}
