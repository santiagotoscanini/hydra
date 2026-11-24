package main

import (
	"strings"
	"testing"

	"github.com/santiagotoscanini/hydra/internal/config"
)

func TestPgbouncerConfig(t *testing.T) {
	st, err := config.Parse([]byte(`
x-hydra: { slot_stride: 200 }
processes:
  supabase:
    command: x
    x-hydra: { kind: supabase, pool: { port: 6432, size: 30 } }
  web:
    command: x
    x-hydra:
      upstreams:
        API: supabase
        SQL: { service: supabase, value: "postgresql://reader:secret@{host}:{port.db}/postgres" }
        ADMIN: { service: supabase, value: "postgresql://postgres@{host}:{port.db}/postgres" }
`))
	if err != nil {
		t.Fatal(err)
	}
	ini, users := pgbouncerConfig(st, st.Pooled(), "/repo/.process-compose/pgbouncer", poolLogins(st, st.Pooled()))
	for _, want := range []string{
		"* = host=127.0.0.1 port=54322",
		"listen_port = 6432",
		"pool_mode = transaction",
		"default_pool_size = 30",
		"max_prepared_statements = 200",
		"auth_file = /repo/.process-compose/pgbouncer/userlist.txt",
	} {
		if !strings.Contains(ini, want) {
			t.Errorf("ini lacks %q:\n%s", want, ini)
		}
	}
	if users != "\"postgres\" \"postgres\"\n\"reader\" \"secret\"\n" {
		t.Errorf("users = %q", users)
	}
}
