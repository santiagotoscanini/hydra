package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/santiagotoscanini/hydra/internal/config"
)

// A local Postgres allows max_connections clients, 100 unless told
// otherwise, and Supabase's own services hold a third of that. Every
// worktree that runs its own copy of a service on main's database adds that
// service's pool: a Go service with a pool of ten, and its worker with ten
// more, leave room for two or three worktrees. The next one fails to connect
// with "remaining connection slots are reserved", which reads like a bug in
// the service. So status says it before that happens.

// dbConns is how full a database is.
type dbConns struct {
	Used, Max int
	Full      bool // a connection was refused for want of one
}

// dbConnsOf asks the Postgres of main's Supabase how many connections are in
// use. ok is false when there is no such database or it can't be asked.
var dbConnsOf = func(st *config.Stack) (dbConns, bool) {
	port := 0
	for _, s := range st.Services {
		if s.Kind == config.KindSupabase {
			port = s.Ports["db"]
		}
	}
	if port == 0 {
		return dbConns{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := fmt.Sprintf("postgresql://postgres:postgres@%s:%d/postgres?sslmode=disable&connect_timeout=2", st.Hydra.Host, port)
	conn, err := pgconn.Connect(ctx, url)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "53300" {
			return dbConns{Full: true}, true
		}
		return dbConns{}, false
	}
	defer conn.Close(context.Background())
	rows, err := conn.Exec(ctx, "select count(*), current_setting('max_connections')::int from pg_stat_activity").ReadAll()
	if err != nil || len(rows) == 0 || len(rows[0].Rows) == 0 {
		return dbConns{}, false
	}
	used, _ := strconv.Atoi(string(rows[0].Rows[0][0]))
	max, _ := strconv.Atoi(string(rows[0].Rows[0][1]))
	return dbConns{Used: used, Max: max}, max > 0
}

// dbConnsProblem is the note for main's database when it is nearly out of
// connections, nil otherwise.
func dbConnsProblem(st *config.Stack) *problem {
	c, ok := dbConnsOf(st)
	if !ok || (!c.Full && c.Used*100 < c.Max*75) {
		return nil
	}
	port := 0
	for _, s := range st.Services {
		if s.Kind == config.KindSupabase {
			port = s.Ports["db"]
		}
	}
	text := fmt.Sprintf("main's database has %d of its %d connections in use.", c.Used, c.Max)
	if c.Full {
		text = "main's database is out of connections: it refused hydra's."
	}
	text += " Each worktree that runs its own api or worker on it takes more, and the next one will fail to connect (\"remaining connection slots are reserved\")."
	// Only supabase_admin may change the setting (Supabase's postgres role is
	// not a superuser), and it takes a restart of the database container.
	// Auth keeps its pool of connections across that and answers every login
	// with "Database error querying schema" until it is restarted too, so the
	// fix restarts both. The data stays: it lives in the container's volume.
	// 300 first; past that, 200 more each time.
	want := 300
	if c.Max >= want {
		want = c.Max + 200
	}
	return &problem{Text: text,
		Fix: fmt.Sprintf(`d=$(docker ps --filter publish=%d --format '{{.Names}}'); docker exec "$d" psql -U supabase_admin -d postgres -c 'alter system set max_connections = %d' && docker restart "$d" "${d/_db_/_auth_}"`, port, want)}
}
