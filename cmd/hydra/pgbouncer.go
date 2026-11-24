package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/plan"
)

// poolCommands are the hydra-pgbouncer process's command and readiness
// check, for the main checkout of a stack with a pool.
func poolCommands(t *target) (run, ready string, err error) {
	exe, err := stableExecutable()
	if err != nil {
		return "", "", err
	}
	args := []string{exe, "pgbouncer", "--root", t.checkout.Root, "-f", t.file}
	for i, a := range args {
		args[i] = shQuote(a)
	}
	cmd := strings.Join(args, " ")
	return "exec " + cmd, cmd + " --ready", nil
}

// pgbouncerCmd runs in the main checkout's process-compose project: it
// writes PgBouncer's configuration for the stack main runs (see
// startedFromPath) and becomes PgBouncer. With --ready it only says whether
// PgBouncer accepts connections.
func pgbouncerCmd(args []string) error {
	fs := flag.NewFlagSet("pgbouncer", flag.ContinueOnError)
	file := fs.String("f", "", "")
	root := fs.String("root", ".", "")
	ready := fs.Bool("ready", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	co, err := gitx.Find(*root)
	if err != nil {
		return err
	}
	if *file == "" {
		*file = defaultFile(co)
	}
	st, err := config.Load(startedFromPath(co.Root))
	if errors.Is(err, os.ErrNotExist) {
		st, err = config.Load(*file)
	}
	if err != nil {
		return err
	}
	s := st.Pooled()
	if s == nil {
		return fmt.Errorf("no service in %s has x-hydra.pool", *file)
	}
	if *ready {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(st.Hydra.Host, strconv.Itoa(s.Pool.Port)), time.Second)
		if err != nil {
			return errExit(1)
		}
		c.Close()
		return nil
	}
	bin, err := exec.LookPath("pgbouncer")
	if err != nil {
		return fmt.Errorf("x-hydra.pool on %s needs PgBouncer 1.21 or later, and it isn't installed (brew install pgbouncer)", s.Name)
	}
	dir := filepath.Join(co.Root, ".process-compose", "pgbouncer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ini, users := pgbouncerConfig(st, s, dir, poolLogins(st, s))
	if err := os.WriteFile(filepath.Join(dir, "userlist.txt"), []byte(users), 0o600); err != nil {
		return err
	}
	path := filepath.Join(dir, "pgbouncer.ini")
	if err := os.WriteFile(path, []byte(ini), 0o644); err != nil {
		return err
	}
	return syscall.Exec(bin, []string{"pgbouncer", path}, os.Environ())
}

// poolLogins are the users PgBouncer logs in to Postgres as, with their
// passwords: postgres/postgres (a local Supabase's), and whoever the stack's
// URLs through the pool name. PgBouncer lets any client in (auth_type trust,
// as a local Postgres does) and logs in to Postgres for it with the password
// listed here, so a user missing from the list can't get a connection.
func poolLogins(st *config.Stack, s *config.Service) map[string]string {
	logins := map[string]string{"postgres": "postgres"}
	for _, name := range st.Names() {
		for _, u := range st.Services[name].Upstreams {
			if u.Service != s.Name {
				continue
			}
			v, err := url.Parse(plan.PooledValue(st, s, u.Value))
			if err != nil || v.User == nil {
				continue
			}
			if pw, ok := v.User.Password(); ok {
				logins[v.User.Username()] = pw
			}
		}
	}
	return logins
}

// pgbouncerConfig is PgBouncer's configuration for the pool of s, and its
// user list.
//
// Transaction pooling is what lets many worktrees share a few connections:
// a connection goes back to the pool when a transaction ends. Prepared
// statements, which pgx uses by default, keep working through it since
// PgBouncer 1.21 (max_prepared_statements). What holds a session doesn't:
// LISTEN, session advisory locks, SET without LOCAL.
func pgbouncerConfig(st *config.Stack, s *config.Service, dir string, logins map[string]string) (ini, users string) {
	p := s.Pool
	ini = fmt.Sprintf(`; Written by hydra for x-hydra.pool on %[1]s. Do not edit.
[databases]
* = host=%[2]s port=%[3]d

[pgbouncer]
listen_addr = %[2]s
listen_port = %[4]d
unix_socket_dir =
auth_type = trust
auth_file = %[5]s
pool_mode = transaction
default_pool_size = %[6]d
max_db_connections = %[6]d
max_client_conn = 5000
max_prepared_statements = 200
ignore_startup_parameters = extra_float_digits
server_idle_timeout = 60
logfile =
pidfile =
log_connections = 0
log_disconnections = 0
`, s.Name, st.Hydra.Host, st.PoolTarget(s), p.Port, filepath.Join(dir, "userlist.txt"), p.Size)
	names := make([]string, 0, len(logins))
	for n := range logins {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		// PgBouncer's quoting: a quote inside is doubled.
		q := func(v string) string { return `"` + strings.ReplaceAll(v, `"`, `""`) + `"` }
		users += q(n) + " " + q(logins[n]) + "\n"
	}
	return ini, users
}
