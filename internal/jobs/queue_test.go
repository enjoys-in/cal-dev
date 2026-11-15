package jobs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestReliableQueue checks that a dequeued job stays in the processing set until acked and is
// re-queued by PromoteScheduled once its visibility deadline passes. Needs a live Valkey/Redis:
// set JOBS_TEST_REDIS_URL (e.g. redis://localhost:6379/15).
func TestReliableQueue(t *testing.T) {
	url := os.Getenv("JOBS_TEST_REDIS_URL")
	if url == "" {
		t.Skip("JOBS_TEST_REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	q := &Queue{client: redis.NewClient(opts), prefix: fmt.Sprintf("jobstest-%d", time.Now().UnixNano()), maxRetry: 3, retryDelay: time.Second}
	defer q.client.Close()
	defer func() {
		keys, _ := q.client.Keys(ctx, q.prefix+":*").Result()
		if len(keys) > 0 {
			q.client.Del(ctx, keys...)
		}
	}()

	id, err := q.Enqueue(ctx, "t", map[string]string{"k": "v"})
	if err != nil {
		t.Fatal(err)
	}

	job, err := q.Dequeue(ctx, time.Second, "other", "t")
	if err != nil || job == nil || job.ID != id {
		t.Fatalf("dequeue: job=%v err=%v", job, err)
	}
	if n := q.client.ZCard(ctx, q.processingKey()).Val(); n != 1 {
		t.Fatalf("processing size = %d, want 1", n)
	}

	// Simulate a crashed worker: the deadline passes without an ack.
	q.client.ZAdd(ctx, q.processingKey(), redis.Z{Score: 1, Member: job.raw})
	if err := q.PromoteScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	if n := q.client.ZCard(ctx, q.processingKey()).Val(); n != 0 {
		t.Fatalf("processing size after reap = %d, want 0", n)
	}

	again, err := q.Dequeue(ctx, time.Second, "t")
	if err != nil || again == nil || again.ID != id {
		t.Fatalf("re-dequeue: job=%v err=%v", again, err)
	}
	if err := q.Ack(ctx, again); err != nil {
		t.Fatal(err)
	}
	if n := q.client.ZCard(ctx, q.processingKey()).Val(); n != 0 {
		t.Fatalf("processing size after ack = %d, want 0", n)
	}
	if job, err := q.Dequeue(ctx, 300*time.Millisecond, "t"); err != nil || job != nil {
		t.Fatalf("queue should be empty: job=%v err=%v", job, err)
	}
}
