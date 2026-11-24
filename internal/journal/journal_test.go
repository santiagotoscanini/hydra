package journal

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEventsFromManyWritersReadBackWhole(t *testing.T) {
	common := t.TempDir()
	Open(common, "fix-cart", 3, "abc1234")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			Log("command", Fields{"args": []string{"status"}, "i": i, "out": strings.Repeat("x", 3000)})
		}(i)
	}
	wg.Wait()
	Log("process", Fields{"name": "api", "t": "shadowed", "tail": strings.Repeat("y", maxString+100)})

	recs, err := Read(common, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 51 {
		t.Fatalf("read %d events, want 51", len(recs))
	}
	last := recs[50]
	if last.Kind != "process" || last.Checkout != "fix-cart" || last.Slot != 3 || last.Str("hydra") != "abc1234" || last.Str("name") != "api" {
		t.Errorf("common fields lost: %s", last.Line)
	}
	if last.Str("_t") != "shadowed" || last.Time.IsZero() {
		t.Errorf("a field named like a common one replaced it: %s", last.Line)
	}
	if n := len(last.Str("tail")); n > maxString+4 {
		t.Errorf("a long string was kept whole (%d bytes)", n)
	}
	if recs, _ := Read(common, time.Now().Add(time.Minute)); len(recs) != 0 {
		t.Errorf("events before since were returned: %d", len(recs))
	}
}

func TestOldDaysArePruned(t *testing.T) {
	d := t.TempDir()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local)
	for _, day := range []string{"2026-10-01", "2026-11-01", "2026-11-24"} {
		os.WriteFile(filepath.Join(d, day+".jsonl"), []byte("{}\n"), 0o644)
	}
	prune(d, now)
	left, _ := filepath.Glob(filepath.Join(d, "*.jsonl"))
	if len(left) != 2 || filepath.Base(left[0]) != "2026-11-01.jsonl" {
		t.Errorf("after pruning: %v", left)
	}
}
