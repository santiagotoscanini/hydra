package tunnels

import (
	"reflect"
	"testing"
	"time"
)

func TestTakeAndRelease(t *testing.T) {
	dir := t.TempDir()
	if held, err := Read(dir); err != nil || len(held) != 0 {
		t.Fatalf("Read on nothing = %v, %v", held, err)
	}
	at := time.Date(2026, 11, 26, 14, 2, 0, 0, time.UTC)
	a := Holder{Root: "/wt/a", Branch: "fix-cart", Since: at}
	prev, err := Take(dir, "hooks", a)
	if err != nil || prev != (Holder{}) {
		t.Fatalf("first take: prev %+v, %v", prev, err)
	}
	// Taking it again keeps the time it was first taken.
	if _, err := Take(dir, "hooks", Holder{Root: "/wt/a", Since: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	b := Holder{Root: "/wt/b", Since: at.Add(time.Minute)}
	if prev, _ = Take(dir, "hooks", b); prev.Root != "/wt/a" || !prev.Since.Equal(at) {
		t.Fatalf("second take: prev %+v", prev)
	}
	if _, err := Take(dir, "other", b); err != nil {
		t.Fatal(err)
	}
	held, _ := Read(dir)
	if len(held) != 2 || held["hooks"].Root != "/wt/b" {
		t.Fatalf("held = %+v", held)
	}
	names, err := ReleaseRoot(dir, "/wt/b")
	if err != nil || !reflect.DeepEqual(names, []string{"hooks", "other"}) {
		t.Fatalf("ReleaseRoot = %v, %v", names, err)
	}
	if names, _ := ReleaseRoot(dir, "/wt/b"); len(names) != 0 {
		t.Fatalf("second ReleaseRoot = %v", names)
	}
	Take(dir, "hooks", a)
	if prev, _ := Release(dir, "hooks"); prev.Root != "/wt/a" {
		t.Fatalf("Release prev = %+v", prev)
	}
	if prev, _ := Release(dir, "hooks"); prev != (Holder{}) {
		t.Fatalf("Release of main's = %+v", prev)
	}
	if held, _ := Read(dir); len(held) != 0 {
		t.Fatalf("left = %+v", held)
	}
}
