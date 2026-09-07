package pairing

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ConfigureDefaultRepository selects a shared Redis authority when Redis is
// configured. With no Redis address the local development behavior remains
// intentionally in-memory.
func ConfigureDefaultRepository(ctx context.Context) (func(), error) {
	if path := strings.TrimSpace(os.Getenv("STACKCHAN_SQLITE_PATH")); path != "" {
		repository, err := NewSQLiteRepository(path, time.Now)
		if err != nil { return nil, fmt.Errorf("open pairing SQLite: %w", err) }
		DefaultRepository = repository
		return func() { _ = repository.Close() }, nil
	}
	address := strings.TrimSpace(os.Getenv("STACKCHAN_REDIS_ADDR"))
	if address == "" {
		DefaultRepository = NewMemoryRepository(time.Now)
		return func() {}, nil
	}
	database, err := redisDatabase()
	if err != nil {
		return nil, err
	}
	addresses := splitAddresses(address)
	if len(addresses) == 0 {
		return nil, fmt.Errorf("STACKCHAN_REDIS_ADDR contains no address")
	}
	client := redis.NewUniversalClient(&redis.UniversalOptions{
		Addrs: addresses, Username: os.Getenv("STACKCHAN_REDIS_USERNAME"),
		Password: os.Getenv("STACKCHAN_REDIS_PASSWORD"), DB: database,
	})
	repository := NewRedisRepository(client, strings.TrimSpace(os.Getenv("STACKCHAN_REDIS_NAMESPACE")))
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := repository.Ping(check); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect to configured pairing Redis: %w", err)
	}
	DefaultRepository = repository
	return func() { _ = client.Close() }, nil
}

func redisDatabase() (int, error) {
	raw := strings.TrimSpace(os.Getenv("STACKCHAN_REDIS_DB"))
	if raw == "" {
		return 0, nil
	}
	database, err := strconv.Atoi(raw)
	if err != nil || database < 0 {
		return 0, fmt.Errorf("STACKCHAN_REDIS_DB must be a non-negative integer")
	}
	return database, nil
}

func splitAddresses(value string) []string {
	var addresses []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			addresses = append(addresses, part)
		}
	}
	return addresses
}
