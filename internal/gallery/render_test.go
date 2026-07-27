package gallery

import (
	"runtime"
	"testing"
	"time"
)

func TestRenderLimiterDefaultsToNumCPU(t *testing.T) {
	g, err := New(Config{PhotoRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := cap(g.renderSem), runtime.NumCPU(); got != want {
		t.Fatalf("default render limit = %d, want %d (NumCPU)", got, want)
	}
}

// TestRenderLimiterBoundsConcurrency verifies the limiter actually blocks once its
// slots are taken, so no more than RenderConcurrency decodes run at once.
func TestRenderLimiterBoundsConcurrency(t *testing.T) {
	g, err := New(Config{PhotoRoot: t.TempDir(), RenderConcurrency: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := cap(g.renderSem); got != 2 {
		t.Fatalf("render limit = %d, want 2", got)
	}

	g.acquireRender()
	g.acquireRender() // both slots now held

	blocked := make(chan struct{})
	go func() {
		g.acquireRender()
		close(blocked)
	}()

	select {
	case <-blocked:
		t.Fatal("a third acquire returned while the limiter was full")
	case <-time.After(50 * time.Millisecond):
		// expected: it blocks
	}

	g.releaseRender() // free one slot

	select {
	case <-blocked:
		// expected: the waiter proceeds
	case <-time.After(time.Second):
		t.Fatal("a third acquire did not proceed after a slot was freed")
	}

	g.releaseRender()
	g.releaseRender()
}
