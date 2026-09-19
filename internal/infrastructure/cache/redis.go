// Package cache provides the short-lived Redis stores: latest market
// prices and today's incomplete daily candles. Everything here fails open —
// a missing or unreachable Redis degrades to direct provider calls, never
// to errors. Durable history lives in the main database instead.
package cache

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/fx"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/config"
)

// Module wires the current-price cache. Without a configured address it
// provides a disabled cache whose reads always miss and writes no-op.
var Module = fx.Module("infrastructure.cache",
	fx.Provide(NewCurrentPriceCache),
)

// NewCurrentPriceCache builds the Redis-backed CurrentPriceCache, or a
// disabled one when no address is configured.
func NewCurrentPriceCache(cfg *config.Config) repository.CurrentPriceCache {
	if cfg == nil || cfg.RedisAddr == "" {
		return disabled{}
	}
	return &Client{rdb: redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})}
}

// Client is a Redis-backed CurrentPriceCache.
type Client struct {
	rdb *redis.Client
}

// Ping verifies Redis answers.
func (c *Client) Ping(ctx context.Context) error {
	if c.rdb == nil {
		return errors.New("cache: redis not configured")
	}
	return c.rdb.Ping(ctx).Err()
}

// Get returns the cached price, or found=false on a miss.
func (c *Client) Get(ctx context.Context, key string) (price float64, found bool, err error) {
	if c.rdb == nil {
		return 0, false, nil
	}
	raw, err := c.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	price, err = strconv.ParseFloat(raw, 64)
	if err != nil {
		// An unparseable entry reads as a miss so callers refetch;
		// surfacing it as an error would fail reads that should degrade
		// to provider calls. See the fail-open contract above.
		return 0, false, nil //nolint:nilerr // corrupt cache entry degrades to a miss by design.
	}
	return price, true, nil
}

// Set stores a price for ttl.
func (c *Client) Set(ctx context.Context, key string, price float64, ttl time.Duration) error {
	if c.rdb == nil {
		return nil
	}
	return c.rdb.Set(ctx, key, strconv.FormatFloat(price, 'f', -1, 64), ttl).Err()
}

// GetRaw returns a cached blob.
func (c *Client) GetRaw(ctx context.Context, key string) (raw []byte, found bool, err error) {
	if c.rdb == nil {
		return nil, false, nil
	}
	raw, err = c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// SetRaw stores a blob for ttl.
func (c *Client) SetRaw(ctx context.Context, key string, raw []byte, ttl time.Duration) error {
	if c.rdb == nil {
		return nil
	}
	return c.rdb.Set(ctx, key, raw, ttl).Err()
}

// Delete drops a key; dropping a missing key is a no-op.
func (c *Client) Delete(ctx context.Context, key string) error {
	if c.rdb == nil {
		return nil
	}
	return c.rdb.Del(ctx, key).Err()
}

// disabled is a CurrentPriceCache without Redis: reads always miss,
// writes no-op. Existing behavior is preserved exactly when no address is
// configured.
type disabled struct{}

// Get always misses: there is no Redis to read from.
func (disabled) Get(context.Context, string) (price float64, found bool, err error) {
	return 0, false, nil
}

// Set discards the write: without Redis there is nothing to persist to.
func (disabled) Set(context.Context, string, float64, time.Duration) error {
	return nil
}

// GetRaw always misses, mirroring Get for blob entries.
func (disabled) GetRaw(context.Context, string) (raw []byte, found bool, err error) {
	return nil, false, nil
}

// SetRaw discards the write, mirroring Set for blob entries.
func (disabled) SetRaw(context.Context, string, []byte, time.Duration) error {
	return nil
}

// Delete is a no-op without Redis.
func (disabled) Delete(context.Context, string) error { return nil }
