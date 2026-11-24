package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// At most N commands of one name run at once; the others wait their turn,
// and a turn given back goes to one of them.
func TestThrottleLetsNRunAtOnce(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	old := throttlePoll
	throttlePoll = 10 * time.Millisecond
	t.Cleanup(func() { throttlePoll = old })

	var running, most atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, _ := takeTurn("go-build", 2)
			now := running.Add(1)
			for {
				m := most.Load()
				if now <= m || most.CompareAndSwap(m, now) {
					break
				}
			}
			time.Sleep(60 * time.Millisecond)
			running.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if most.Load() != 2 {
		t.Errorf("at most %d ran at once, want 2", most.Load())
	}
	// Another name has turns of its own.
	a, _ := takeTurn("go-build", 1)
	b, waited := takeTurn("other", 1)
	if waited > 50*time.Millisecond {
		t.Errorf("a different name waited %s", waited)
	}
	a()
	b()
}
