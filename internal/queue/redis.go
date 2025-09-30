package queue

import (
    "context"
    "errors"
    "fmt"
    "strings"
    "time"

    redis "github.com/redis/go-redis/v9"
)

// RedisQueue implements Redis Streams + consumer groups with a delayed ZSET mover.
type RedisQueue struct {
    client       *redis.Client
    // streams / groups
    Stream       string
    Group        string
    // keys
    CancelKey    string
    DelayedKey   string
    DLQStream    string
    IdemDoneKey  string
    // mover control
    pollInterval time.Duration
    stop         chan struct{}
}

// NewRedisQueue connects to Redis, ensures stream & group, and starts delayed mover.
func NewRedisQueue(redisURL, stream, group string, poll time.Duration) (*RedisQueue, error) {
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
    q := &RedisQueue{
        client:       c,
        Stream:       stream,
        Group:        group,
        CancelKey:    "jobs:cancelled:set",
        DelayedKey:   stream + ":delayed",
        DLQStream:    stream + ":dlq",
        IdemDoneKey:  "idem:done:",
        pollInterval: poll,
        stop:         make(chan struct{}),
    }
    // Ensure consumer group exists (MKSTREAM creates stream if missing)
    if err := c.XGroupCreateMkStream(ctx, stream, group, "$"
    ).Err(); err != nil && !isBusyGroupErr(err) {
        return nil, fmt.Errorf("xgroup create: %w", err)
    }
    // Start delayed mover
    go q.mover()
    return q, nil
}

func isBusyGroupErr(err error) bool {
    if err == nil { return false }
    if errors.Is(err, redis.ErrBusyGroup) { return true }
    // go-redis may return a generic error string from Redis
    return strings.Contains(strings.ToUpper(err.Error()), "BUSYGROUP")
}

func (q *RedisQueue) Close() error {
    close(q.stop)
    return q.client.Close()
}

// Ping checks redis connectivity.
func (q *RedisQueue) Ping(ctx context.Context) error { return q.client.Ping(ctx).Err() }

// EnqueueAI adds a job to the stream as a single-field entry {data: <json>}.
func (q *RedisQueue) EnqueueAI(ctx context.Context, payload []byte) error {
    return q.client.XAdd(ctx, &redis.XAddArgs{
        Stream: q.Stream,
        Values: map[string]any{"data": string(payload)},
    }).Err()
}

// EnqueueDelayed schedules a job for later execution via ZSET.
func (q *RedisQueue) EnqueueDelayed(ctx context.Context, payload []byte, executeAt time.Time) error {
    return q.client.ZAdd(ctx, q.DelayedKey, redis.Z{Score: float64(executeAt.Unix()), Member: string(payload)}).Err()
}

// DequeueAI reads one message from the consumer group and ACKs it immediately.
// Note: ack-on-read mimics BRPOP semantics; retry/DLQ will be handled at higher layers.
func (q *RedisQueue) DequeueAI(ctx context.Context, consumer string, timeout time.Duration) (string, []byte, error) {
    res, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
        Group:    q.Group,
        Consumer: consumer,
        Streams:  []string{q.Stream, ">"},
        Count:    1,
        Block:    timeout,
        NoAck:    false,
    }).Result()
    if err != nil {
        if err == redis.Nil { return "", nil, nil }
        return "", nil, err
    }
    if len(res) == 0 || len(res[0].Messages) == 0 { return "", nil, nil }
    msg := res[0].Messages[0]
    if v, ok := msg.Values["data"]; ok {
        switch t := v.(type) {
        case string:
            return msg.ID, []byte(t), nil
        case []byte:
            return msg.ID, t, nil
        }
    }
    return msg.ID, nil, nil
}

// Ack marks a message as processed.
func (q *RedisQueue) Ack(ctx context.Context, msgID string) error {
    if msgID == "" { return nil }
    return q.client.XAck(ctx, q.Stream, q.Group, msgID).Err()
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

// AddDLQ pushes a failed job to DLQ stream with reason.
func (q *RedisQueue) AddDLQ(ctx context.Context, payload []byte, reason string) error {
    return q.client.XAdd(ctx, &redis.XAddArgs{Stream: q.DLQStream, Values: map[string]any{"data": string(payload), "reason": reason}}).Err()
}

// IsIdemDone returns true if idempotency key already marked done.
func (q *RedisQueue) IsIdemDone(ctx context.Context, key string) (bool, error) {
    if key == "" { return false, nil }
    exists, err := q.client.Exists(ctx, q.IdemDoneKey+key).Result()
    return exists == 1, err
}

// MarkIdemDone marks idempotency key as done with TTL.
func (q *RedisQueue) MarkIdemDone(ctx context.Context, key string, ttl time.Duration) error {
    if key == "" { return nil }
    return q.client.Set(ctx, q.IdemDoneKey+key, 1, ttl).Err()
}

// mover periodically moves due delayed jobs from ZSET into the stream.
func (q *RedisQueue) mover() {
    if q.pollInterval <= 0 { q.pollInterval = 200 * time.Millisecond }
    ticker := time.NewTicker(q.pollInterval)
    defer ticker.Stop()
    for {
        select {
        case <-q.stop:
            return
        case <-ticker.C:
            q.moveOnce()
        }
    }
}

func (q *RedisQueue) moveOnce() {
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    now := time.Now().Unix()
    // Fetch up to 100 ready items
    vals, err := q.client.ZRangeByScoreWithScores(ctx, q.DelayedKey, &redis.ZRangeBy{
        Min: "-inf", Max: fmt.Sprintf("%d", now), Offset: 0, Count: 100,
    }).Result()
    if err != nil || len(vals) == 0 { return }
    // pipeline add+rem
    pipe := q.client.TxPipeline()
    for _, z := range vals {
        s, _ := z.Member.(string)
        pipe.XAdd(ctx, &redis.XAddArgs{Stream: q.Stream, Values: map[string]any{"data": s}})
        pipe.ZRem(ctx, q.DelayedKey, s)
    }
    _, _ = pipe.Exec(ctx)
}

// Depths returns approximate stream/deferred/dlq lengths for metrics.
func (q *RedisQueue) Depths(ctx context.Context) (int64, int64, int64, error) {
    pipe := q.client.Pipeline()
    xlen := pipe.XLen(ctx, q.Stream)
    zcard := pipe.ZCard(ctx, q.DelayedKey)
    dxlen := pipe.XLen(ctx, q.DLQStream)
    _, err := pipe.Exec(ctx)
    if err != nil { return 0, 0, 0, err }
    return xlen.Val(), zcard.Val(), dxlen.Val(), nil
}
