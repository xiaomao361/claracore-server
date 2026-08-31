package main

import (
	"testing"
	"time"
)

func TestIndexerLeaseTTLCoversEmbeddingTimeoutAndCleanup(t *testing.T) {
	if got := indexerLeaseTTL(30 * time.Second); got != 65*time.Second {
		t.Fatalf("租约应覆盖两倍模型超时与写回余量，实际 %s", got)
	}
	if got := indexerLeaseTTL(0); got != 65*time.Second {
		t.Fatalf("非法超时应使用安全默认值，实际 %s", got)
	}
}
