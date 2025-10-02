package dispatcher

import (
	"context"
	"fmt"
	"strconv"
	"time"

	redis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

// CircuitBreaker manages circuit breaker state in Redis
type CircuitBreaker struct {
	redis         *redis.Client
	baseBackoff   time.Duration
	maxBackoff    time.Duration
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(redisClient *redis.Client, baseBackoff, maxBackoff time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		redis:       redisClient,
		baseBackoff: baseBackoff,
		maxBackoff:  maxBackoff,
	}
}

// OpenCircuitBreaker opens the circuit breaker for a provider:model combination
func (cb *CircuitBreaker) OpenCircuitBreaker(ctx context.Context, provider, model string) {
	key := fmt.Sprintf("cb:%s:%s", provider, model)

	// Get current failure count
	failuresStr, _ := cb.redis.HGet(ctx, key, "failures").Result()
	failures, _ := strconv.Atoi(failuresStr)
	failures++

	// Exponential backoff: 30s, 60s, 120s, 240s, max 5m
	backoff := cb.baseBackoff
	for i := 1; i < failures; i++ {
		backoff *= 2
		if backoff > cb.maxBackoff {
			backoff = cb.maxBackoff
			break
		}
	}

	retryAt := time.Now().Add(backoff).Unix()
	openedAt := time.Now().Unix()

	// Write to Redis
	cb.redis.HSet(ctx, key, map[string]interface{}{
		"state":     "open",
		"retry_at":  retryAt,
		"failures":  failures,
		"opened_at": openedAt,
	})
	cb.redis.Expire(ctx, key, 10*time.Minute)

	log.Warn().
		Str("provider", provider).
		Str("model", model).
		Dur("cooldown", backoff).
		Int("failures", failures).
		Time("retry_at", time.Unix(retryAt, 0)).
		Msg("circuit breaker OPENED")
}

// IsCircuitOpen checks if circuit breaker is open for a provider:model
func (cb *CircuitBreaker) IsCircuitOpen(ctx context.Context, provider, model string) bool {
	key := fmt.Sprintf("cb:%s:%s", provider, model)

	// Get state
	state, err := cb.redis.HGet(ctx, key, "state").Result()
	if err != nil || state == "" {
		// No breaker record → closed by default
		return false
	}

	if state != "open" {
		// Already closed or half-open
		return false
	}

	// Check if cooldown expired
	retryAtStr, _ := cb.redis.HGet(ctx, key, "retry_at").Result()
	retryAt, _ := strconv.ParseInt(retryAtStr, 10, 64)

	if time.Now().Unix() >= retryAt {
		// Cooldown expired → move to half-open
		cb.redis.HSet(ctx, key, "state", "half_open")

		log.Info().
			Str("provider", provider).
			Str("model", model).
			Msg("circuit breaker moved to HALF-OPEN")

		return false // Allow probni request
	}

	// Still in cooldown
	return true
}

// CloseCircuitBreaker closes (resets) the circuit breaker on success
func (cb *CircuitBreaker) CloseCircuitBreaker(ctx context.Context, provider, model string) {
	key := fmt.Sprintf("cb:%s:%s", provider, model)

	// Get current state
	state, _ := cb.redis.HGet(ctx, key, "state").Result()

	if state == "" || state == "closed" {
		// Already closed
		return
	}

	// Reset breaker
	cb.redis.Del(ctx, key)

	log.Info().
		Str("provider", provider).
		Str("model", model).
		Msg("circuit breaker CLOSED (reset)")
}
