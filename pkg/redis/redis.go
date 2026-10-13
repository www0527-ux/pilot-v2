package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"shanyangcode.com/pilot/internal/config"
)

// Client 是可注入的 Redis 客户端封装。
type Client struct {
	rdb *redis.Client
}

// New 创建 Redis 客户端并验证连通性。
func New(ctx context.Context, cfg config.Redis) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		PoolSize: cfg.PoolSize,
	})

	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis connection failed: %w", err)
	}
	return &Client{rdb: rdb}, nil
}

// SetJSON 存储 JSON 数据
func (c *Client) SetJSON(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal json failed: %w", err)
	}
	return c.rdb.Set(ctx, key, data, expiration).Err()
}

// GetJSON 获取 JSON 数据
func (c *Client) GetJSON(ctx context.Context, key string, dest interface{}) error {
	data, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dest)
}

// CompareAndSwapJSONStatus atomically changes the top-level string `status`
// field of a JSON value. It is used by persisted workflow state machines to
// prevent concurrent double-resume.
func (c *Client) CompareAndSwapJSONStatus(ctx context.Context, key, expected, replacement string, expiration time.Duration) (bool, error) {
	if c == nil || c.rdb == nil {
		return false, fmt.Errorf("redis client unavailable")
	}
	swapped := false
	err := c.rdb.Watch(ctx, func(tx *redis.Tx) error {
		raw, err := tx.Get(ctx, key).Bytes()
		if err != nil {
			return err
		}
		var value map[string]interface{}
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		status, _ := value["status"].(string)
		if status != expected {
			return nil
		}
		value["status"] = replacement
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, key, encoded, expiration)
			return nil
		})
		if err == nil {
			swapped = true
		}
		return err
	}, key)
	return swapped, err
}

// DeleteKeys 删除多个 key
func (c *Client) DeleteKeys(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.rdb.Del(ctx, keys...).Err()
}

// Exists 检查 key 是否存在
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	count, err := c.rdb.Exists(ctx, key).Result()
	return count > 0, err
}

// ScanKeys 使用 SCAN 遍历匹配的 key，避免生产环境使用 KEYS 阻塞 Redis。
func (c *Client) ScanKeys(ctx context.Context, pattern string, count int64) ([]string, error) {
	if count <= 0 {
		count = 100
	}

	keys := make([]string, 0)
	var cursor uint64
	for {
		batch, nextCursor, err := c.rdb.Scan(ctx, cursor, pattern, count).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		cursor = nextCursor
		if cursor == 0 {
			break
		}
	}

	return keys, nil
}

// Close 关闭连接池。
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Ping 探测 Redis 连通性，供 readiness 使用。
func (c *Client) Ping(ctx context.Context) error {
	if c == nil || c.rdb == nil {
		return fmt.Errorf("redis client is nil")
	}
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping failed: %w", err)
	}
	return nil
}
