package assemblyai

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

var (
	ErrInvalidJobToken = errors.New("invalid speaker identification job token")
	ErrExpiredJobToken = errors.New("speaker identification job token expired")
)

type jobTokenClaims struct {
	TranscriptID string `json:"tid"`
	UserID       string `json:"uid"`
	ExpiresAt    int64  `json:"exp"`
}

type JobTokenSigner struct {
	Secret []byte
}

func (s JobTokenSigner) Sign(transcriptID, userID string, expiresAt time.Time) (string, error) {
	if len(s.Secret) < 32 || strings.TrimSpace(transcriptID) == "" || strings.TrimSpace(userID) == "" {
		return "", ErrInvalidJobToken
	}
	payload, err := json.Marshal(jobTokenClaims{
		TranscriptID: transcriptID,
		UserID:       userID,
		ExpiresAt:    expiresAt.Unix(),
	})
	if err != nil {
		return "", ErrInvalidJobToken
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(s.signature([]byte(encoded))), nil
}

func (s JobTokenSigner) Verify(token, userID string, now time.Time) (string, error) {
	if len(s.Secret) < 32 || strings.TrimSpace(userID) == "" {
		return "", ErrInvalidJobToken
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return "", ErrInvalidJobToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(signature, s.signature([]byte(parts[0]))) {
		return "", ErrInvalidJobToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", ErrInvalidJobToken
	}
	var claims jobTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.TranscriptID == "" || claims.UserID != userID {
		return "", ErrInvalidJobToken
	}
	if now.Unix() > claims.ExpiresAt {
		return "", ErrExpiredJobToken
	}
	return claims.TranscriptID, nil
}

func (s JobTokenSigner) signature(payload []byte) []byte {
	mac := hmac.New(sha256.New, s.Secret)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}
