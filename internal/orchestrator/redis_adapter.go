package orchestrator

import (
	"context"
	"time"

	redis "github.com/redis/go-redis/v9"
)

// redisClientAdapter wraps *redis.Client to match RedisClient interface
type redisClientAdapter struct {
	client *redis.Client
}

func NewRedisAdapter(client *redis.Client) RedisClient {
	return &redisClientAdapter{client: client}
}

func (r *redisClientAdapter) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) StatusCmd {
	return r.client.Set(ctx, key, value, expiration)
}

func (r *redisClientAdapter) Get(ctx context.Context, key string) StringCmd {
	return r.client.Get(ctx, key)
}
