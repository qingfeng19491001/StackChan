package pairing

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gogf/gf/v2/net/ghttp"
	"github.com/golang-jwt/jwt/v5"
)

type JWTAuthenticator struct {
	Issuer, Audience, JWKSURL string
	HTTPClient                *http.Client
	Now                       func() time.Time
	mu                        sync.Mutex
	keys                      map[string]*rsa.PublicKey
	keysUntil                 time.Time
}

type jwkSet struct {
	Keys []struct{ Kty, Kid, Use, Alg, N, E string } `json:"keys"`
}

func (a *JWTAuthenticator) Authenticate(r *ghttp.Request) (string, error) {
	return a.AuthenticateHeader(r.Header.Get("Authorization"))
}

func (a *JWTAuthenticator) AuthenticateHeader(header string) (string, error) {
	if !strings.HasPrefix(header, "Bearer ") {
		return "", ErrUnauthorizedJWT
	}
	tokenText := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	claims := &jwt.RegisteredClaims{}
	keyFunc := func(token *jwt.Token) (any, error) {
		if token.Method.Alg() != jwt.SigningMethodRS256.Alg() {
			return nil, ErrUnauthorizedJWT
		}
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, ErrUnauthorizedJWT
		}
		return a.key(kid)
	}
	now := a.Now
	if now == nil {
		now = time.Now
	}
	parsed, err := jwt.ParseWithClaims(tokenText, claims, keyFunc, jwt.WithIssuer(a.Issuer), jwt.WithAudience(a.Audience), jwt.WithExpirationRequired(), jwt.WithTimeFunc(now), jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}))
	if err != nil || !parsed.Valid || claims.Subject == "" {
		return "", ErrUnauthorizedJWT
	}
	return claims.Subject, nil
}

var ErrUnauthorizedJWT = errors.New("UNAUTHORIZED")

func (a *JWTAuthenticator) key(kid string) (*rsa.PublicKey, error) {
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if key := a.keys[kid]; key != nil && now.Before(a.keysUntil) {
		return key, nil
	}
	if err := a.refreshLocked(now); err != nil {
		return nil, err
	}
	key := a.keys[kid]
	if key == nil {
		return nil, ErrUnauthorizedJWT
	}
	return key, nil
}

func (a *JWTAuthenticator) refreshLocked(now time.Time) error {
	client := a.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	response, err := client.Get(a.JWKSURL)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrUnauthorizedJWT
	}
	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&set); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, item := range set.Keys {
		if item.Kty != "RSA" || item.Kid == "" || (item.Alg != "" && item.Alg != "RS256") {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(item.N)
		if err != nil {
			continue
		}
		eBytes, err := base64.RawURLEncoding.DecodeString(item.E)
		if err != nil {
			continue
		}
		e := 0
		for _, value := range eBytes {
			e = e<<8 + int(value)
		}
		if e > 0 {
			keys[item.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: e}
		}
	}
	if len(keys) == 0 {
		return ErrUnauthorizedJWT
	}
	a.keys, a.keysUntil = keys, now.Add(5*time.Minute)
	return nil
}
