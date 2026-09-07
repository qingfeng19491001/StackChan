package pairing

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidMAC        = errors.New("invalid MAC")
	ErrNonceInvalid      = errors.New("pairing nonce invalid")
	ErrNonceExpired      = errors.New("pairing nonce expired")
	ErrConnectionChanged = errors.New("device connection changed")
	ErrAlreadyBound      = errors.New("device already bound")
	ErrNotBound          = errors.New("NOT_BOUND")
	ErrTicketInvalid     = errors.New("websocket ticket invalid")
	ErrTicketExpired     = errors.New("websocket ticket expired")
)

const (
	NonceTTL  = 2 * time.Minute
	TicketTTL = time.Minute
)

type PairNonce struct {
	Value, MAC string
	ExpiresAt  int64
}
type Device struct {
	MAC     string
	BoundAt int64
}
type Ticket struct {
	Value     string
	ExpiresAt int64
}
type TicketClaims struct {
	UserID, MAC, Role, DeviceID string
	ExpiresAt                   int64
}

// Repository is the complete pairing authority used by HTTP handlers and the
// meeting WebSocket. Keeping this small interface makes the single-instance
// memory implementation and the production Redis implementation behave the
// same at the security boundary.
type Repository interface {
	IssueNonce(mac string, generation uint64) (PairNonce, error)
	Bind(userID, mac, nonce string, generation uint64) error
	Devices(userID string) []Device
	OwnerOf(mac string) (string, bool)
	Unbind(userID, mac string) bool
	IssueTicket(userID, mac, role, deviceID string) (Ticket, error)
	ConsumeTicket(ticket string) (TicketClaims, error)
}
type nonceRecord struct {
	MAC        string
	Generation uint64
	ExpiresAt  time.Time
}

type MemoryRepository struct {
	mu      sync.Mutex
	now     func() time.Time
	nonces  map[[32]byte]nonceRecord
	tickets map[[32]byte]TicketClaims
	owners  map[string]string
	devices map[string]map[string]Device
}

var DefaultRepository Repository = NewMemoryRepository(time.Now)

func NewMemoryRepository(now func() time.Time) *MemoryRepository {
	if now == nil {
		now = time.Now
	}
	return &MemoryRepository{now: now, nonces: make(map[[32]byte]nonceRecord), tickets: make(map[[32]byte]TicketClaims), owners: make(map[string]string), devices: make(map[string]map[string]Device)}
}

func NormalizeMAC(value string) (string, error) {
	normalized := strings.ToUpper(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(value)))
	if !regexp.MustCompile(`^[0-9A-F]{12}$`).MatchString(normalized) {
		return "", ErrInvalidMAC
	}
	return normalized, nil
}

func randomSecret() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func secretHash(value string) [32]byte { return sha256.Sum256([]byte(value)) }

func (r *MemoryRepository) IssueNonce(mac string, generation uint64) (PairNonce, error) {
	mac, err := NormalizeMAC(mac)
	if err != nil {
		return PairNonce{}, err
	}
	if generation == 0 {
		return PairNonce{}, ErrConnectionChanged
	}
	secret, err := randomSecret()
	if err != nil {
		return PairNonce{}, err
	}
	expires := r.now().Add(NonceTTL)
	r.mu.Lock()
	r.nonces[secretHash(secret)] = nonceRecord{MAC: mac, Generation: generation, ExpiresAt: expires}
	r.mu.Unlock()
	return PairNonce{Value: secret, MAC: mac, ExpiresAt: expires.Unix()}, nil
}

func (r *MemoryRepository) Bind(userID, mac, nonce string, generation uint64) error {
	if userID == "" {
		return ErrNonceInvalid
	}
	mac, err := NormalizeMAC(mac)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := secretHash(nonce)
	record, ok := r.nonces[key]
	if !ok || record.MAC != mac {
		return ErrNonceInvalid
	}
	if r.now().After(record.ExpiresAt) {
		delete(r.nonces, key)
		return ErrNonceExpired
	}
	if record.Generation != generation {
		return ErrConnectionChanged
	}
	if owner := r.owners[mac]; owner != "" && owner != userID {
		return ErrAlreadyBound
	}
	delete(r.nonces, key)
	r.owners[mac] = userID
	if r.devices[userID] == nil {
		r.devices[userID] = make(map[string]Device)
	}
	r.devices[userID][mac] = Device{MAC: mac, BoundAt: r.now().Unix()}
	return nil
}

func (r *MemoryRepository) Devices(userID string) []Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]Device, 0, len(r.devices[userID]))
	for _, device := range r.devices[userID] {
		result = append(result, device)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].BoundAt < result[j].BoundAt })
	return result
}

func (r *MemoryRepository) OwnerOf(mac string) (string, bool) {
	mac, err := NormalizeMAC(mac)
	if err != nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	owner := r.owners[mac]
	return owner, owner != ""
}

func (r *MemoryRepository) Unbind(userID, mac string) bool {
	mac, err := NormalizeMAC(mac)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners[mac] != userID {
		return false
	}
	delete(r.owners, mac)
	delete(r.devices[userID], mac)
	return true
}

func (r *MemoryRepository) IssueTicket(userID, mac, role, deviceID string) (Ticket, error) {
	mac, err := NormalizeMAC(mac)
	if err != nil {
		return Ticket{}, err
	}
	if role != "app" || deviceID == "" {
		return Ticket{}, ErrTicketInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.owners[mac] != userID {
		return Ticket{}, ErrNotBound
	}
	secret, err := randomSecret()
	if err != nil {
		return Ticket{}, err
	}
	expires := r.now().Add(TicketTTL)
	r.tickets[secretHash(secret)] = TicketClaims{UserID: userID, MAC: mac, Role: role, DeviceID: deviceID, ExpiresAt: expires.Unix()}
	return Ticket{Value: secret, ExpiresAt: expires.Unix()}, nil
}

func (r *MemoryRepository) ConsumeTicket(ticket string) (TicketClaims, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := secretHash(ticket)
	claims, ok := r.tickets[key]
	if !ok {
		return TicketClaims{}, ErrTicketInvalid
	}
	delete(r.tickets, key)
	if r.now().Unix() > claims.ExpiresAt {
		return TicketClaims{}, ErrTicketExpired
	}
	if r.owners[claims.MAC] != claims.UserID {
		return TicketClaims{}, ErrNotBound
	}
	return claims, nil
}
