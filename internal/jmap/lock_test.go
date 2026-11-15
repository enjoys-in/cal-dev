package jmap

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestLockAccountInProcessFallback(t *testing.T) {
	h := &Handler{}
	unlock, err := h.lockAccount(context.Background(), 900001, 900001)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestLockAccountRedisDownFailsAndReleases(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 200 * time.Millisecond})
	defer c.Close()
	h := &Handler{locker: c, lockPrefix: "test"}
	if _, err := h.lockAccount(context.Background(), 900002, 900002); err == nil {
		t.Fatal("expected an error when Redis is unreachable")
	}
	// The in-process mutex must have been released.
	unlock, err := (&Handler{}).lockAccount(context.Background(), 900002, 900002)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

// testRedis returns a client for TEST_REDIS_URL (default localhost:6379), or skips.
func testRedis(t *testing.T) *redis.Client {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		url = "redis://localhost:6379/0"
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Skip(err)
	}
	c := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		c.Close()
		t.Skipf("no Redis: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestLockAccountRedis(t *testing.T) {
	c := testRedis(t)
	ctx := context.Background()
	prefix := "caldev-test-" + uuid.NewString()
	key := fmt.Sprintf("%s:jmap:lock:%d:%d", prefix, 900003, 900003)
	defer c.Del(ctx, key)
	h := &Handler{locker: c, lockPrefix: prefix}

	// Another instance holds the lock briefly: we wait for it, then get it.
	c.Set(ctx, key, "other-instance", 300*time.Millisecond)
	start := time.Now()
	unlock, err := h.lockAccount(ctx, 900003, 900003)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 200*time.Millisecond {
		t.Fatal("lock taken while another instance held it")
	}
	if ttl := c.PTTL(ctx, key).Val(); ttl <= 0 || ttl > setLockTTL {
		t.Fatalf("lock must have a TTL, got %s", ttl)
	}
	unlock()
	if n := c.Exists(ctx, key).Val(); n != 0 {
		t.Fatal("unlock must delete our lock")
	}

	// Our lock expired and another instance took it: unlock must not delete theirs.
	unlock, err = h.lockAccount(ctx, 900003, 900003)
	if err != nil {
		t.Fatal(err)
	}
	c.Set(ctx, key, "other-instance", time.Minute)
	unlock()
	if v := c.Get(ctx, key).Val(); v != "other-instance" {
		t.Fatalf("unlock deleted another holder's lock (value %q)", v)
	}
}
