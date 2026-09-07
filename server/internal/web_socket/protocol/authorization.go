/*
SPDX-FileCopyrightText: 2026 M5Stack Technology CO LTD
SPDX-License-Identifier: MIT
*/

package protocol

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidAuthorizationToken = errors.New("invalid authorization token")
	ErrAuthorizationTokenExpired = errors.New("authorization token expired or not yet valid")
)

func ParseAuthorizationClaims(token string, now time.Time) (string, error) {
	parts := strings.Split(token, "|")
	if len(parts) != 3 || parts[0] == "" {
		return "", ErrInvalidAuthorizationToken
	}
	timestamp, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", ErrInvalidAuthorizationToken
	}
	delta := now.Unix() - timestamp
	if delta > 10 || delta < -10 {
		return "", ErrAuthorizationTokenExpired
	}
	return parts[0], nil
}
