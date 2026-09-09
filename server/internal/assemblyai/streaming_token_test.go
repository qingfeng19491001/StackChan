package assemblyai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateStreamingToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.Header.Get("Authorization") != "test-key" {
			t.Fatalf("authorization header was not forwarded")
		}
		if r.URL.Query().Get("expires_in_seconds") != "60" {
			t.Fatalf("missing expires_in_seconds")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"temporary","expires_in_seconds":60}`))
	}))
	defer server.Close()

	service := &Service{APIKey: "test-key", Endpoint: server.URL, HTTPClient: server.Client()}
	token, err := service.CreateStreamingToken(context.Background(), 60, 600)
	if err != nil {
		t.Fatalf("CreateStreamingToken() error = %v", err)
	}
	if token.Token != "temporary" || token.ExpiresInSeconds != 60 || token.MaxSessionDurationSeconds != 600 {
		t.Fatalf("unexpected token response: %+v", token)
	}
}

func TestCreateStreamingTokenRequiresAPIKey(t *testing.T) {
	service := &Service{Endpoint: "http://unused"}
	_, err := service.CreateStreamingToken(context.Background(), 60, 600)
	if err != ErrAPIKeyNotConfigured {
		t.Fatalf("error = %v, want ErrAPIKeyNotConfigured", err)
	}
}
