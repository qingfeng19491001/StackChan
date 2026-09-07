package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"stackChan/internal/pairing"
)

type RedisSessionRecord struct {
	MAC            string `json:"mac"`
	SessionID      string `json:"sessionId"`
	StartCommandID string `json:"startCommandId,omitempty"`
	StopCommandID  string `json:"stopCommandId,omitempty"`
	State          string `json:"state"`
	Owner          Owner  `json:"owner"`
	NextSequence   uint32 `json:"nextSequence"`
	AudioReceived  bool   `json:"audioReceived"`
	ExpiresAt      int64  `json:"expiresAt,omitempty"`
	UpdatedAt      int64  `json:"updatedAt"`
}

type RedisReplayFrame struct {
	Sequence     uint32 `json:"sequence"`
	Payload      []byte `json:"payload"`
	ReceivedAtMS int64  `json:"receivedAtMs"`
	Size         int    `json:"size"`
}

type RedisCommandRecord struct {
	Action, MAC, SessionID, CommandID, State string
	Owner                                    Owner
	UpdatedAt                                int64
}

type RedisSessionStore struct {
	client    redis.UniversalClient
	namespace string
	ttl       time.Duration
}

const offerExpiryCleanupGrace = 5 * time.Second

func NewRedisSessionStore(client redis.UniversalClient, namespace string, ttl time.Duration) *RedisSessionStore {
	if namespace == "" {
		namespace = "stackchan"
	}
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return &RedisSessionStore{client: client, namespace: namespace, ttl: ttl}
}

func (s *RedisSessionStore) key(mac string) string {
	return fmt.Sprintf("%s:meeting:mac:%s", s.namespace, mac)
}

func (s *RedisSessionStore) replayKey(mac string) string {
	return fmt.Sprintf("%s:meeting:replay:%s", s.namespace, mac)
}

func (s *RedisSessionStore) replayBytesKey(mac string) string {
	return fmt.Sprintf("%s:meeting:replay-bytes:%s", s.namespace, mac)
}

func (s *RedisSessionStore) commandKey(action, mac, sessionID, commandID string) string {
	return fmt.Sprintf("%s:meeting:command:%s:%s:%s:%s", s.namespace, action, mac, sessionID, commandID)
}

func (s *RedisSessionStore) normalize(record RedisSessionRecord) (RedisSessionRecord, error) {
	mac, err := pairing.NormalizeMAC(record.MAC)
	if err != nil || record.SessionID == "" {
		return RedisSessionRecord{}, ErrSessionNotFound
	}
	record.MAC = mac
	record.UpdatedAt = time.Now().Unix()
	return record, nil
}

func (s *RedisSessionStore) Claim(ctx context.Context, record RedisSessionRecord) error {
	if s == nil || s.client == nil {
		return errors.New("Redis session store is not configured")
	}
	record, err := s.normalize(record)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	ttl := s.ttl
	if record.State == "offered" && record.ExpiresAt != 0 {
		ttl = time.Until(time.Unix(record.ExpiresAt, 0)) + offerExpiryCleanupGrace
		if ttl <= 0 {
			return ErrStartTimeout
		}
	}
	claimed, err := s.client.SetNX(ctx, s.key(record.MAC), encoded, ttl).Result()
	if err != nil {
		return err
	}
	if !claimed {
		return ErrSessionBusy
	}
	// A previous session's short-lived replay keys may still exist after an
	// unclean shutdown. A successful new claim is the safe point to clear them.
	if err := s.client.Del(ctx, s.replayKey(record.MAC), s.replayBytesKey(record.MAC)).Err(); err != nil {
		_, _ = s.client.Del(ctx, s.key(record.MAC)).Result()
		return err
	}
	return nil
}

var appendReplayScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 0 end
local session = cjson.decode(current)
if session.sessionId ~= ARGV[1] then return -1 end
redis.call('RPUSH', KEYS[2], ARGV[2])
local total = redis.call('INCRBY', KEYS[3], ARGV[3])
while redis.call('LLEN', KEYS[2]) > 0 do
  local first = cjson.decode(redis.call('LINDEX', KEYS[2], 0))
  if tonumber(first.receivedAtMs) >= tonumber(ARGV[4]) and total <= tonumber(ARGV[5]) then break end
  redis.call('LPOP', KEYS[2])
  total = redis.call('DECRBY', KEYS[3], tonumber(first.size))
end
redis.call('PEXPIRE', KEYS[2], ARGV[6])
redis.call('PEXPIRE', KEYS[3], ARGV[6])
return 1
`)

func (s *RedisSessionStore) AppendReplay(ctx context.Context, mac, sessionID string, frame RedisReplayFrame) error {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil || sessionID == "" || len(frame.Payload) == 0 {
		return ErrSessionNotFound
	}
	frame.Size = len(frame.Payload)
	encoded, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	result, err := appendReplayScript.Run(ctx, s.client, []string{
		s.key(mac), s.replayKey(mac), s.replayBytesKey(mac),
	}, sessionID, encoded, frame.Size, frame.ReceivedAtMS-audioReplayWindow.Milliseconds(), audioReplayMaxBytes, (audioReplayWindow + offerExpiryCleanupGrace).Milliseconds()).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return ErrSessionNotFound
	}
	if result < 0 {
		return ErrSessionBusy
	}
	return nil
}

func (s *RedisSessionStore) LoadReplay(ctx context.Context, mac, sessionID string, since time.Time) ([]RedisReplayFrame, error) {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil || sessionID == "" {
		return nil, ErrSessionNotFound
	}
	record, err := s.Load(ctx, mac)
	if err != nil {
		return nil, err
	}
	if record == nil || record.SessionID != sessionID {
		return nil, ErrSessionNotFound
	}
	values, err := s.client.LRange(ctx, s.replayKey(mac), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	frames := make([]RedisReplayFrame, 0, len(values))
	cutoff := since.UnixMilli()
	for _, value := range values {
		var frame RedisReplayFrame
		if err := json.Unmarshal([]byte(value), &frame); err != nil {
			return nil, err
		}
		if frame.ReceivedAtMS >= cutoff {
			frames = append(frames, frame)
		}
	}
	return frames, nil
}

func (s *RedisSessionStore) SaveCommand(ctx context.Context, record RedisCommandRecord) error {
	mac, err := pairing.NormalizeMAC(record.MAC)
	if err != nil || record.Action == "" || record.SessionID == "" || record.CommandID == "" {
		return ErrSessionNotFound
	}
	record.MAC = mac
	record.UpdatedAt = time.Now().Unix()
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return s.client.SetNX(ctx, s.commandKey(record.Action, mac, record.SessionID, record.CommandID), encoded, s.ttl).Err()
}

func (s *RedisSessionStore) LoadCommand(ctx context.Context, action, mac, sessionID, commandID string) (*RedisCommandRecord, error) {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil || action == "" || sessionID == "" || commandID == "" {
		return nil, ErrSessionNotFound
	}
	encoded, err := s.client.Get(ctx, s.commandKey(action, mac, sessionID, commandID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record RedisCommandRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

var promoteOfferScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 0 end
local decoded = cjson.decode(current)
if decoded.state ~= 'offered' or decoded.sessionId ~= ARGV[1] or decoded.startCommandId ~= ARGV[2] then return -1 end
redis.call('SET', KEYS[1], ARGV[3], 'PX', ARGV[4])
return 1
`)

// PromoteOffer atomically assigns one Auro owner to a device-created offer.
// This is the distributed arbitration point when the device and Auro sockets
// terminate on different Server instances.
func (s *RedisSessionStore) PromoteOffer(ctx context.Context, record RedisSessionRecord) (bool, error) {
	record, err := s.normalize(record)
	if err != nil {
		return false, err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return false, err
	}
	result, err := promoteOfferScript.Run(
		ctx,
		s.client,
		[]string{s.key(record.MAC)},
		record.SessionID,
		record.StartCommandID,
		encoded,
		s.ttl.Milliseconds(),
	).Int()
	if err != nil {
		return false, err
	}
	// A non-offer record means another instance already promoted or replaced
	// this offer. Let the manager load it and distinguish idempotency from a
	// competing owner.
	if result < 0 {
		return false, nil
	}
	return result == 1, nil
}

var releaseOfferScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 0 end
local decoded = cjson.decode(current)
if decoded.state ~= 'offered' or decoded.sessionId ~= ARGV[1] or decoded.startCommandId ~= ARGV[2] then return 0 end
return redis.call('DEL', KEYS[1])
`)

func (s *RedisSessionStore) ReleaseOffer(ctx context.Context, mac, sessionID, commandID string) (bool, error) {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return false, err
	}
	result, err := releaseOfferScript.Run(
		ctx, s.client, []string{s.key(mac)}, sessionID, commandID,
	).Int()
	return result == 1, err
}

var saveSessionScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 0 end
local decoded = cjson.decode(current)
if decoded.sessionId ~= ARGV[1] then return -1 end
redis.call('SET', KEYS[1], ARGV[2], 'PX', ARGV[3])
return 1
`)

func (s *RedisSessionStore) Save(ctx context.Context, record RedisSessionRecord) error {
	record, err := s.normalize(record)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	result, err := saveSessionScript.Run(ctx, s.client, []string{s.key(record.MAC)}, record.SessionID, encoded, s.ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return ErrSessionNotFound
	}
	if result < 0 {
		return ErrSessionBusy
	}
	return nil
}

func (s *RedisSessionStore) Load(ctx context.Context, mac string) (*RedisSessionRecord, error) {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return nil, err
	}
	encoded, err := s.client.Get(ctx, s.key(mac)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record RedisSessionRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return nil, err
	}
	return &record, nil
}

var releaseSessionScript = redis.NewScript(`
local current = redis.call('GET', KEYS[1])
if not current then return 0 end
local decoded = cjson.decode(current)
if decoded.sessionId ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
return 1
`)

func (s *RedisSessionStore) Release(ctx context.Context, mac, sessionID string) (bool, error) {
	mac, err := pairing.NormalizeMAC(mac)
	if err != nil {
		return false, err
	}
	result, err := releaseSessionScript.Run(ctx, s.client, []string{
		s.key(mac), s.replayKey(mac), s.replayBytesKey(mac),
	}, sessionID).Int()
	return result == 1, err
}

func (s *RedisSessionStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}
