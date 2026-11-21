package glob

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"apps/api/**", "apps/api/main.go", true},
		{"apps/api/**", "apps/api/internal/x/y.go", true},
		{"apps/api/**", "apps/apix/main.go", false},
		{"apps/api/", "apps/api/main.go", true},
		{"**/*.sql", "db/migrations/001.sql", true},
		{"**/*.sql", "001.sql", true},
		{"packages/*/package.json", "packages/ui/package.json", true},
		{"packages/*/package.json", "packages/ui/src/package.json", false},
		{"go.mod", "go.mod", true},
		{"go.mod", "apps/go.mod", false},
		{"packages/{names,phone}/**", "packages/phone/src/a.ts", true},
		{"packages/{names,phone}/**", "packages/pricing/src/a.ts", false},
	}
	for _, c := range cases {
		if got := Match(c.pattern, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
