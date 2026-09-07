package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisRepository persists pair bindings and one-time secrets so a load
// balanced Server has one authorization authority. Secrets are keyed by their
// SHA-256 digest, exactly as in MemoryRepository; raw nonce/ticket values are
// never stored or logged.
type RedisRepository struct {
	client    redis.UniversalClient
	namespace string
	now       func() time.Time
}

func NewRedisRepository(client redis.UniversalClient, namespace string) *RedisRepository {
	if namespace == "" {
		namespace = "stackchan"
	}
	return &RedisRepository{client: client, namespace: namespace, now: time.Now}
}

func (r *RedisRepository) nonceKey(secret string) string {
	return fmt.Sprintf("%s:pairing:nonce:%x", r.namespace, secretHash(secret))
}
func (r *RedisRepository) ticketKey(secret string) string {
	return fmt.Sprintf("%s:pairing:ticket:%x", r.namespace, secretHash(secret))
}
func (r *RedisRepository) ownerKey(mac string) string {
	return fmt.Sprintf("%s:pairing:owner:%s", r.namespace, mac)
}
func (r *RedisRepository) devicesKey(userID string) string {
	return fmt.Sprintf("%s:pairing:devices:%x", r.namespace, secretHash(userID))
}

func (r *RedisRepository) IssueNonce(mac string, generation uint64) (PairNonce, error) {
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
	payload, err := json.Marshal(nonceRecord{MAC: mac, Generation: generation, ExpiresAt: expires})
	if err != nil {
		return PairNonce{}, err
	}
	if err := r.client.Set(context.Background(), r.nonceKey(secret), payload, NonceTTL).Err(); err != nil {
		return PairNonce{}, err
	}
	return PairNonce{Value: secret, MAC: mac, ExpiresAt: expires.Unix()}, nil
}

func (r *RedisRepository) Bind(userID, mac, nonce string, generation uint64) error {
	if userID == "" {
		return ErrNonceInvalid
	}
	mac, err := NormalizeMAC(mac)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for attempt := 0; attempt != 3; attempt++ {
		err = r.client.Watch(ctx, func(tx *redis.Tx) error {
			encoded, getErr := tx.Get(ctx, r.nonceKey(nonce)).Bytes()
			if errors.Is(getErr, redis.Nil) {
				return ErrNonceInvalid
			}
			if getErr != nil {
				return getErr
			}
			var record nonceRecord
			if json.Unmarshal(encoded, &record) != nil || record.MAC != mac {
				return ErrNonceInvalid
			}
			if r.now().After(record.ExpiresAt) {
				return ErrNonceExpired
			}
			if record.Generation != generation {
				return ErrConnectionChanged
			}
			owner, ownerErr := tx.Get(ctx, r.ownerKey(mac)).Result()
			if ownerErr != nil && !errors.Is(ownerErr, redis.Nil) {
				return ownerErr
			}
			if owner != "" && owner != userID {
				return ErrAlreadyBound
			}
			_, pipeErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Set(ctx, r.ownerKey(mac), userID, 0)
				pipe.ZAdd(ctx, r.devicesKey(userID), redis.Z{Score: float64(r.now().Unix()), Member: mac})
				pipe.Del(ctx, r.nonceKey(nonce))
				return nil
			})
			return pipeErr
		}, r.nonceKey(nonce), r.ownerKey(mac))
		if !errors.Is(err, redis.TxFailedErr) {
			return err
		}
	}
	return ErrNonceInvalid
}

func (r *RedisRepository) Devices(userID string) []Device {
	if userID == "" {
		return nil
	}
	entries, err := r.client.ZRangeWithScores(context.Background(), r.devicesKey(userID), 0, -1).Result()
	if err != nil {
		return nil
	}
	devices := make([]Device, 0, len(entries))
	for _, entry := range entries {
		mac, ok := entry.Member.(string)
		if !ok {
			continue
		}
		if owner, bound := r.OwnerOf(mac); !bound || owner != userID {
			continue
		}
		devices = append(devices, Device{MAC: mac, BoundAt: int64(entry.Score)})
	}
	return devices
}

func (r *RedisRepository) OwnerOf(mac string) (string, bool) {
	mac, err := NormalizeMAC(mac)
	if err != nil {
		return "", false
	}
	owner, err := r.client.Get(context.Background(), r.ownerKey(mac)).Result()
	return owner, err == nil && owner != ""
}

func (r *RedisRepository) Unbind(userID, mac string) bool {
	mac, err := NormalizeMAC(mac)
	if err != nil || userID == "" {
		return false
	}
	ctx := context.Background()
	for attempt := 0; attempt != 3; attempt++ {
		err = r.client.Watch(ctx, func(tx *redis.Tx) error {
			owner, getErr := tx.Get(ctx, r.ownerKey(mac)).Result()
			if getErr != nil || owner != userID {
				return ErrNotBound
			}
			_, pipeErr := tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Del(ctx, r.ownerKey(mac))
				pipe.ZRem(ctx, r.devicesKey(userID), mac)
				return nil
			})
			return pipeErr
		}, r.ownerKey(mac))
		if err == nil {
			return true
		}
		if !errors.Is(err, redis.TxFailedErr) {
			return false
		}
	}
	return false
}

func (r *RedisRepository) IssueTicket(userID, mac, role, deviceID string) (Ticket, error) {
	mac, err := NormalizeMAC(mac)
	if err != nil {
		return Ticket{}, err
	}
	if role != "app" || deviceID == "" {
		return Ticket{}, ErrTicketInvalid
	}
	owner, bound := r.OwnerOf(mac)
	if !bound || owner != userID {
		return Ticket{}, ErrNotBound
	}
	secret, err := randomSecret()
	if err != nil {
		return Ticket{}, err
	}
	expires := r.now().Add(TicketTTL)
	payload, err := json.Marshal(TicketClaims{UserID: userID, MAC: mac, Role: role, DeviceID: deviceID, ExpiresAt: expires.Unix()})
	if err != nil {
		return Ticket{}, err
	}
	if err := r.client.Set(context.Background(), r.ticketKey(secret), payload, TicketTTL).Err(); err != nil {
		return Ticket{}, err
	}
	return Ticket{Value: secret, ExpiresAt: expires.Unix()}, nil
}

var consumeTicketScript = redis.NewScript(`
local value = redis.call('GET', KEYS[1])
if value then redis.call('DEL', KEYS[1]) end
return value
`)

func (r *RedisRepository) ConsumeTicket(ticket string) (TicketClaims, error) {
	value, err := consumeTicketScript.Run(context.Background(), r.client, []string{r.ticketKey(ticket)}).Result()
	encoded, ok := value.(string)
	if err != nil || !ok || encoded == "" {
		return TicketClaims{}, ErrTicketInvalid
	}
	var claims TicketClaims
	if json.Unmarshal([]byte(encoded), &claims) != nil {
		return TicketClaims{}, ErrTicketInvalid
	}
	if r.now().Unix() > claims.ExpiresAt {
		return TicketClaims{}, ErrTicketExpired
	}
	owner, bound := r.OwnerOf(claims.MAC)
	if !bound || owner != claims.UserID {
		return TicketClaims{}, ErrNotBound
	}
	return claims, nil
}

func (r *RedisRepository) Ping(ctx context.Context) error {
	if r == nil || r.client == nil {
		return errors.New("Redis pairing repository is not configured")
	}
	return r.client.Ping(ctx).Err()
}
