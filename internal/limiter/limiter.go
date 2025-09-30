package limiter

import (
    "context"
    "fmt"
    "strings"
    "sync"
    "time"

    redis "github.com/redis/go-redis/v9"
)

type Adaptive struct {
    rdb        *redis.Client
    maxInflight int
    baseBackoff time.Duration
    maxBackoff  time.Duration
    mu         sync.Mutex
    sem        map[string]chan struct{}
}

type Options struct {
    RedisURL    string
    MaxInflight int
    BaseBackoff time.Duration
    MaxBackoff  time.Duration
}

func New(opts Options) (*Adaptive, error) {
    if opts.MaxInflight <= 0 { opts.MaxInflight = 2 }
    if opts.BaseBackoff <= 0 { opts.BaseBackoff = 30 * time.Second }
    if opts.MaxBackoff <= 0 { opts.MaxBackoff = 5 * time.Minute }
    ro, err := redis.ParseURL(opts.RedisURL)
    if err != nil { return nil, err }
    c := redis.NewClient(ro)
    if err := c.Ping(context.Background()).Err(); err != nil { return nil, err }
    return &Adaptive{rdb: c, maxInflight: opts.MaxInflight, baseBackoff: opts.BaseBackoff, maxBackoff: opts.MaxBackoff, sem: map[string]chan struct{}{}}, nil
}

func (a *Adaptive) key(provider, model string) string {
    return fmt.Sprintf("cb:%s:%s", strings.ToLower(provider), strings.ToLower(model))
}

// IsOpen returns true if breaker is open (cooldown active).
func (a *Adaptive) IsOpen(ctx context.Context, provider, model string) bool {
    k := a.key(provider, model)
    ts, err := a.rdb.Get(ctx, k).Int64()
    if err != nil { return false }
    return time.Now().Unix() < ts
}

// Open sets/extends the cooldown with exponential backoff per attempt.
func (a *Adaptive) Open(ctx context.Context, provider, model string) {
    k := a.key(provider, model)
    cntKey := k + ":attempts"
    // increment attempts and compute backoff
    attempts, _ := a.rdb.Incr(ctx, cntKey).Result()
    if attempts < 1 { attempts = 1 }
    // backoff doubles up to maxBackoff
    d := a.baseBackoff * (1 << (attempts - 1))
    if d > a.maxBackoff { d = a.maxBackoff }
    until := time.Now().Add(d).Unix()
    _ = a.rdb.Set(ctx, k, until, d).Err()
}

// Close resets the breaker for provider/model.
func (a *Adaptive) Close(ctx context.Context, provider, model string) {
    k := a.key(provider, model)
    _ = a.rdb.Del(ctx, k, k+":attempts").Err()
}

// Allow tries to reserve a local in-process slot for provider:model.
// Returns a release function and true if allowed; otherwise nil,false.
func (a *Adaptive) Allow(provider, model string) (func(), bool) {
    key := strings.ToLower(provider) + ":" + strings.ToLower(model)
    a.mu.Lock()
    ch, ok := a.sem[key]
    if !ok {
        ch = make(chan struct{}, a.maxInflight)
        a.sem[key] = ch
    }
    a.mu.Unlock()
    select {
    case ch <- struct{}{}:
        return func() { <-ch }, true
    default:
        return func(){}, false
    }
}

func (a *Adaptive) CloseClient() error { return a.rdb.Close() }

