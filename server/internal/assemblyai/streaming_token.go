package assemblyai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

const tokenEndpoint = "https://streaming.assemblyai.com/v3/token"

var ErrAPIKeyNotConfigured = errors.New("ASSEMBLYAI_API_KEY is not configured")

type Token struct {
	Token                     string `json:"token"`
	ExpiresInSeconds          int    `json:"expires_in_seconds"`
	MaxSessionDurationSeconds int    `json:"max_session_duration_seconds"`
}

type tokenResponse struct {
	Token            string `json:"token"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

type Service struct {
	APIKey     string
	HTTPClient *http.Client
	Endpoint   string
}

func NewService() *Service {
	return &Service{
		APIKey:   os.Getenv("ASSEMBLYAI_API_KEY"),
		Endpoint: tokenEndpoint,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// CreateStreamingToken creates a one-time token. The permanent API key must
// only be available to this server and is never included in the response.
func (s *Service) CreateStreamingToken(ctx context.Context, expiresIn, maxSessionDuration int) (Token, error) {
	if s.APIKey == "" {
		return Token{}, ErrAPIKeyNotConfigured
	}
	if expiresIn < 1 || expiresIn > 600 {
		return Token{}, fmt.Errorf("expires_in_seconds must be between 1 and 600")
	}
	if maxSessionDuration < 60 || maxSessionDuration > 10800 {
		return Token{}, fmt.Errorf("max_session_duration_seconds must be between 60 and 10800")
	}

	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return Token{}, fmt.Errorf("parse AssemblyAI endpoint: %w", err)
	}
	query := u.Query()
	query.Set("expires_in_seconds", strconv.Itoa(expiresIn))
	query.Set("max_session_duration_seconds", strconv.Itoa(maxSessionDuration))
	u.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Token{}, fmt.Errorf("create AssemblyAI request: %w", err)
	}
	req.Header.Set("Authorization", s.APIKey)
	req.Header.Set("Accept", "application/json")

	client := s.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("request AssemblyAI token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Token{}, fmt.Errorf("read AssemblyAI response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("AssemblyAI token request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Token{}, fmt.Errorf("decode AssemblyAI token response: %w", err)
	}
	if parsed.Token == "" {
		return Token{}, errors.New("AssemblyAI token response did not contain token")
	}
	return Token{
		Token:                     parsed.Token,
		ExpiresInSeconds:          parsed.ExpiresInSeconds,
		MaxSessionDurationSeconds: maxSessionDuration,
	}, nil
}
