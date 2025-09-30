package queue

import (
    "context"
    "fmt"
    "time"

    redis "github.com/redis/go-redis/v9"
)

// Minimal Redis list-based queue for PoC. TODO: Switch to Streams + consumer groups.

type RedisQueue struct {
    client *redis.Client
    // keys
    AIJobsKey string
    CancelKey string
}

func NewRedisQueue(redisURL string) (*RedisQueue, error) {
    opt, err := redis.ParseURL(redisURL)
    if err != nil {
        return nil, fmt.Errorf("parse redis url: %w", err)
    }
    c := redis.NewClient(opt)
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    if err := c.Ping(ctx).Err(); err != nil {
        return nil, fmt.Errorf("redis ping: %w", err)
    }
    return &RedisQueue{client: c, AIJobsKey: "jobs:ai:pages:list", CancelKey: "jobs:cancelled:set"}, nil
}

func (q *RedisQueue) Close() error { return q.client.Close() }

// EnqueueAI enqueues a serialized payload for AI processing.
func (q *RedisQueue) EnqueueAI(ctx context.Context, payload []byte) error {
    return q.client.LPush(ctx, q.AIJobsKey, payload).Err()
}

// DequeueAI blocks for up to timeout to pop a job.
func (q *RedisQueue) DequeueAI(ctx context.Context, timeout time.Duration) ([]byte, error) {
    res, err := q.client.BRPop(ctx, timeout, q.AIJobsKey).Result()
    if err != nil {
        if err == redis.Nil {
            return nil, nil
        }
        return nil, err
    }
    if len(res) != 2 { return nil, nil }
    return []byte(res[1]), nil
}

// CancelJob marks a job as cancelled. Workers should check this before processing.
func (q *RedisQueue) CancelJob(ctx context.Context, jobID string) error {
    return q.client.SAdd(ctx, q.CancelKey, jobID).Err()
}

// IsCancelled returns true if job is cancelled.
func (q *RedisQueue) IsCancelled(ctx context.Context, jobID string) (bool, error) {
    res, err := q.client.SIsMember(ctx, q.CancelKey, jobID).Result()
    return res, err
}
