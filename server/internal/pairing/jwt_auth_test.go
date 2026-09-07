package pairing

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWTAuthenticatorVerifiesJWKSClaims(t *testing.T) {
	privateKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	kid := "key-1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		e := big.NewInt(int64(privateKey.PublicKey.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(e)}}})
	}))
	defer server.Close()
	now := time.Unix(1_700_000_000, 0)
	auth := &JWTAuthenticator{Issuer: "https://project.supabase.co/auth/v1", Audience: "authenticated", JWKSURL: server.URL, Now: func() time.Time { return now }}
	claims := jwt.RegisteredClaims{Subject: "user-1", Issuer: auth.Issuer, Audience: jwt.ClaimStrings{auth.Audience}, ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)), IssuedAt: jwt.NewNumericDate(now)}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, _ := token.SignedString(privateKey)
	if user, err := auth.AuthenticateHeader("Bearer " + signed); err != nil || user != "user-1" {
		t.Fatalf("user=%q err=%v", user, err)
	}
	if _, err := auth.AuthenticateHeader("Bearer " + signed + "broken"); err == nil {
		t.Fatal("invalid signature accepted")
	}
}
