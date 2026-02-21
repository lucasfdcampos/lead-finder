// Package cache provides a thin Redis client wrapper used by provider-level
// cache decorators throughout the application.
package cache

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client wraps a Redis connection and exposes typed Get/Set helpers that
// marshal/unmarshal values as JSON. All operations are best-effort: errors are
// silently swallowed so the caller can always fall through to the real provider.
type Client struct {
	rdb *redis.Client
}

// NewClient dials addr (e.g. "localhost:6379") and returns a ready Client.
// The connection is lazy — no error is returned at construction time.
func NewClient(addr string) *Client {
	return &Client{
		rdb: redis.NewClient(&redis.Options{Addr: addr}),
	}
}

// Get unmarshals the JSON value stored at key into dest.
// Returns true on a cache hit, false on any miss or error.
func (c *Client) Get(ctx context.Context, key string, dest any) bool {
	b, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return false
	}
	return json.Unmarshal(b, dest) == nil
}

// Set marshals val to JSON and stores it under key with the given TTL.
// Errors are silently ignored.
func (c *Client) Set(ctx context.Context, key string, val any, ttl time.Duration) {
	b, err := json.Marshal(val)
	if err != nil {
		return
	}
	c.rdb.Set(ctx, key, b, ttl) //nolint:errcheck
}

// Ping verifies the connection is alive. Useful for startup health-checks.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close releases the underlying connection.
func (c *Client) Close() error {
	return c.rdb.Close()
}
