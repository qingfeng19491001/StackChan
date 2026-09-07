package web_socket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"stackChan/internal/meeting"
)

// MeetingCluster relays delivery commands, never raw client credentials, to
// the Server instance that owns the relevant WebSocket. Redis is optional:
// without it all delivery remains strictly local for development/MVP.
type MeetingCluster interface {
	NodeID() string
	Publish(context.Context, clusterDelivery) error
	RegisterDevice(context.Context, string) (uint64, error)
	TouchDevice(context.Context, string, uint64) error
	UnregisterDevice(context.Context, string, uint64) error
	DeviceGeneration(context.Context, string) (uint64, bool)
	Close() error
}

type clusterDeliveryKind string

const (
	clusterToDevice    clusterDeliveryKind = "device"
	clusterToBoundApps clusterDeliveryKind = "bound-apps"
	clusterToOwner     clusterDeliveryKind = "owner"
	clusterAudioOwner  clusterDeliveryKind = "owner-audio"
)

type clusterDelivery struct {
	Origin  string                  `json:"origin"`
	Kind    clusterDeliveryKind     `json:"kind"`
	MAC     string                  `json:"mac,omitempty"`
	Owner   meeting.Owner           `json:"owner,omitempty"`
	Control *meeting.ControlMessage `json:"control,omitempty"`
	Audio   []byte                  `json:"audio,omitempty"`
}

type localMeetingCluster struct{}

func (localMeetingCluster) NodeID() string { return "" }
func (localMeetingCluster) Publish(context.Context, clusterDelivery) error {
	return errors.New("meeting cluster routing is not configured")
}
func (localMeetingCluster) RegisterDevice(context.Context, string) (uint64, error) {
	return 0, errors.New("meeting cluster routing is not configured")
}
func (localMeetingCluster) TouchDevice(context.Context, string, uint64) error {
	return errors.New("meeting cluster routing is not configured")
}
func (localMeetingCluster) UnregisterDevice(context.Context, string, uint64) error {
	return errors.New("meeting cluster routing is not configured")
}
func (localMeetingCluster) DeviceGeneration(context.Context, string) (uint64, bool) {
	return 0, false
}
func (localMeetingCluster) Close() error { return nil }

type redisMeetingCluster struct {
	client  redis.UniversalClient
	channel string
	nodeID  string
	pubsub  *redis.PubSub
	cancel  context.CancelFunc
	done    chan struct{}
}

func (r *redisMeetingCluster) NodeID() string { return r.nodeID }

const devicePresenceTTL = 45 * time.Second

func (r *redisMeetingCluster) devicePresenceKey(mac string) string {
	return r.channel + ":device:" + mac
}

func (r *redisMeetingCluster) deviceGenerationKey(mac string) string {
	return r.channel + ":device-generation:" + mac
}

func (r *redisMeetingCluster) RegisterDevice(ctx context.Context, mac string) (uint64, error) {
	generation, err := r.client.Incr(ctx, r.deviceGenerationKey(mac)).Uint64()
	if err != nil {
		return 0, err
	}
	if err := r.client.Set(ctx, r.devicePresenceKey(mac), generation, devicePresenceTTL).Err(); err != nil {
		return 0, err
	}
	return generation, nil
}

var touchDeviceScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
return redis.call('PEXPIRE', KEYS[1], ARGV[2])
`)

func (r *redisMeetingCluster) TouchDevice(ctx context.Context, mac string, generation uint64) error {
	result, err := touchDeviceScript.Run(ctx, r.client, []string{r.devicePresenceKey(mac)}, generation, devicePresenceTTL.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return errors.New("device presence generation replaced")
	}
	return nil
}

var unregisterDeviceScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
return redis.call('DEL', KEYS[1])
`)

func (r *redisMeetingCluster) UnregisterDevice(ctx context.Context, mac string, generation uint64) error {
	return unregisterDeviceScript.Run(ctx, r.client, []string{r.devicePresenceKey(mac)}, generation).Err()
}

func (r *redisMeetingCluster) DeviceGeneration(ctx context.Context, mac string) (uint64, bool) {
	generation, err := r.client.Get(ctx, r.devicePresenceKey(mac)).Uint64()
	return generation, err == nil
}

func (r *redisMeetingCluster) Publish(ctx context.Context, delivery clusterDelivery) error {
	delivery.Origin = r.nodeID
	payload, err := json.Marshal(delivery)
	if err != nil {
		return err
	}
	return r.client.Publish(ctx, r.channel, payload).Err()
}

func (r *redisMeetingCluster) Close() error {
	r.cancel()
	_ = r.pubsub.Close()
	<-r.done
	return r.client.Close()
}

var (
	meetingClusterMu sync.RWMutex
	meetingCluster   MeetingCluster = localMeetingCluster{}
)

func currentMeetingCluster() MeetingCluster {
	meetingClusterMu.RLock()
	defer meetingClusterMu.RUnlock()
	return meetingCluster
}

func currentMeetingNodeID() string { return currentMeetingCluster().NodeID() }

func ConfigureMeetingCluster(ctx context.Context) (func(), error) {
	address := strings.TrimSpace(os.Getenv("STACKCHAN_REDIS_ADDR"))
	meetingClusterMu.Lock()
	old := meetingCluster
	meetingCluster = localMeetingCluster{}
	meetingClusterMu.Unlock()
	_ = old.Close()
	if address == "" {
		return func() {}, nil
	}
	database, err := clusterRedisDatabase()
	if err != nil {
		return nil, err
	}
	addresses := clusterAddresses(address)
	if len(addresses) == 0 {
		return nil, errors.New("STACKCHAN_REDIS_ADDR contains no address")
	}
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: addresses, Username: os.Getenv("STACKCHAN_REDIS_USERNAME"),
		Password: os.Getenv("STACKCHAN_REDIS_PASSWORD"), DB: database,
	})
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(check).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect to configured meeting cluster Redis: %w", err)
	}
	namespace := strings.TrimSpace(os.Getenv("STACKCHAN_REDIS_NAMESPACE"))
	if namespace == "" {
		namespace = "stackchan"
	}
	nodeID := strings.TrimSpace(os.Getenv("STACKCHAN_NODE_ID"))
	if nodeID == "" {
		nodeID = uuid.NewString()
	}
	runContext, runCancel := context.WithCancel(context.Background())
	cluster := &redisMeetingCluster{
		client: client, channel: namespace + ":meeting:deliveries", nodeID: nodeID,
		cancel: runCancel, done: make(chan struct{}),
	}
	cluster.pubsub = client.Subscribe(runContext, cluster.channel)
	if _, err := cluster.pubsub.ReceiveTimeout(check, 5*time.Second); err != nil {
		runCancel()
		_ = cluster.pubsub.Close()
		_ = client.Close()
		return nil, fmt.Errorf("subscribe to meeting cluster Redis: %w", err)
	}
	go cluster.listen(runContext)
	meetingClusterMu.Lock()
	meetingCluster = cluster
	meetingClusterMu.Unlock()
	return func() {
		meetingClusterMu.Lock()
		if meetingCluster == cluster {
			meetingCluster = localMeetingCluster{}
		}
		meetingClusterMu.Unlock()
		_ = cluster.Close()
	}, nil
}

func (r *redisMeetingCluster) listen(ctx context.Context) {
	defer close(r.done)
	channel := r.pubsub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case message, ok := <-channel:
			if !ok {
				return
			}
			var delivery clusterDelivery
			if json.Unmarshal([]byte(message.Payload), &delivery) != nil || delivery.Origin == r.nodeID {
				continue
			}
			deliverClusterMessage(context.Background(), delivery)
		}
	}
}

func deliverClusterMessage(ctx context.Context, delivery clusterDelivery) {
	switch delivery.Kind {
	case clusterToDevice:
		if delivery.Control != nil {
			_ = sendDeviceLocal(ctx, delivery.MAC, *delivery.Control)
		}
	case clusterToBoundApps:
		if delivery.Control != nil {
			_ = sendBoundAppsLocal(ctx, delivery.MAC, *delivery.Control)
		}
	case clusterToOwner:
		if delivery.Control != nil {
			_ = sendOwnerLocal(ctx, delivery.Owner, *delivery.Control)
		}
	case clusterAudioOwner:
		_ = sendOwnerAudioLocal(ctx, delivery.Owner, delivery.Audio)
	}
}

func clusterRedisDatabase() (int, error) {
	raw := strings.TrimSpace(os.Getenv("STACKCHAN_REDIS_DB"))
	if raw == "" {
		return 0, nil
	}
	database, err := strconv.Atoi(raw)
	if err != nil || database < 0 {
		return 0, errors.New("STACKCHAN_REDIS_DB must be a non-negative integer")
	}
	return database, nil
}

func clusterAddresses(value string) []string {
	var addresses []string
	for _, value := range strings.Split(value, ",") {
		if value = strings.TrimSpace(value); value != "" {
			addresses = append(addresses, value)
		}
	}
	return addresses
}
