package presence

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	presenceKeyPrefix = "presence:"
	presenceTTL       = 90 * time.Second // 3x heartbeat interval
)

// Info represents a user's presence information
type Info struct {
	UserID    string
	GatewayID string
	ConnID    string
	Timestamp int64
}

// Manager handles user presence using Redis
type Manager struct {
	redis *redis.Client
}

// NewManager creates a new presence manager
func NewManager(redisClient *redis.Client) *Manager {
	return &Manager{
		redis: redisClient,
	}
}

// Register registers a user's presence with CAS (Compare-And-Set) to handle race conditions
func (m *Manager) Register(ctx context.Context, userID, gatewayID, connID string) error {
	key := presenceKeyPrefix + userID
	timestamp := time.Now().Unix()

	// Lua script to ensure atomic update with timestamp check
	script := `
		local key = KEYS[1]
		local new_gw = ARGV[1]
		local new_conn = ARGV[2]
		local new_ts = tonumber(ARGV[3])
		local ttl = tonumber(ARGV[4])

		local current_ts = redis.call('HGET', key, 'ts')

		-- Only update if this is newer than existing record
		if current_ts and tonumber(current_ts) > new_ts then
			return 0
		end

		redis.call('HSET', key, 'gwId', new_gw, 'connId', new_conn, 'ts', new_ts)
		redis.call('EXPIRE', key, ttl)
		return 1
	`

	result, err := m.redis.Eval(ctx, script, []string{key},
		gatewayID, connID, timestamp, int(presenceTTL.Seconds())).Result()

	if err != nil {
		return fmt.Errorf("failed to register presence: %w", err)
	}

	if result.(int64) == 0 {
		return fmt.Errorf("stale update rejected for user %s", userID)
	}

	return nil
}

// Refresh updates the TTL for a user's presence (heartbeat)
func (m *Manager) Refresh(ctx context.Context, userID string) error {
	key := presenceKeyPrefix + userID

	// Update timestamp and refresh TTL
	timestamp := time.Now().Unix()
	pipe := m.redis.Pipeline()
	pipe.HSet(ctx, key, "ts", timestamp)
	pipe.Expire(ctx, key, presenceTTL)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to refresh presence: %w", err)
	}

	return nil
}

// Get retrieves a user's presence information
func (m *Manager) Get(ctx context.Context, userID string) (*Info, error) {
	key := presenceKeyPrefix + userID

	result, err := m.redis.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get presence: %w", err)
	}

	if len(result) == 0 {
		return nil, fmt.Errorf("user %s is offline", userID)
	}

	timestamp := int64(0)
	if ts, ok := result["ts"]; ok {
		fmt.Sscanf(ts, "%d", &timestamp)
	}

	return &Info{
		UserID:    userID,
		GatewayID: result["gwId"],
		ConnID:    result["connId"],
		Timestamp: timestamp,
	}, nil
}

// Remove deletes a user's presence (on disconnect)
func (m *Manager) Remove(ctx context.Context, userID string) error {
	key := presenceKeyPrefix + userID

	err := m.redis.Del(ctx, key).Err()
	if err != nil {
		return fmt.Errorf("failed to remove presence: %w", err)
	}

	return nil
}

// IsOnline checks if a user is currently online
func (m *Manager) IsOnline(ctx context.Context, userID string) (bool, error) {
	key := presenceKeyPrefix + userID

	exists, err := m.redis.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("failed to check presence: %w", err)
	}

	return exists > 0, nil
}
