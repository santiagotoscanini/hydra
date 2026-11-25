package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/santiagotoscanini/hydra/internal/config"
	"github.com/santiagotoscanini/hydra/internal/gitx"
	"github.com/santiagotoscanini/hydra/internal/pc"
	"github.com/santiagotoscanini/hydra/internal/render"
)

// A worktree's own Supabase with x-hydra.seed_from_main starts from main's
// data rather than the seed files: the branch's migrations then run on real
// rows, and one that breaks on them fails here, not after the merge.
//
// The start that creates the stack runs without migrations and seeds (the
// rendered command exports SUPABASE_DB_MIGRATIONS_ENABLED=false while
// render.SeedPending is there). After it, seedFromMain
//
//  1. applies the migrations main's database has, from this worktree's files,
//     so the schema is main's;
//  2. copies main's rows in: every table of the app's schemas, auth and
//     storage, and the sequences, in one transaction;
//  3. leaves the branch's own migrations to the step that always follows a
//     start (`supabase migration up`), which now runs them on main's rows.
//
// The copy goes table by table over COPY between the two servers, so no
// pg_dump of the right version is needed, and nothing is written to disk.

// seedSkipSchemas hold what Supabase's services and extensions keep for
// themselves, not the app's data: the worktree's stack has its own, made by
// the same CLI. Migrations that create cron jobs or vault secrets create
// them here too.
var seedSkipSchemas = []string{
	"information_schema", "pg_catalog", "pg_toast",
	"supabase_migrations", // set by step 1
	"extensions", "graphql", "graphql_public", "pgbouncer", "pgsodium", "pgsodium_masks", "vault",
	"realtime", "_realtime", "supabase_functions", "net", "cron", "_analytics", "_supavisor",
}

// seedSkipTables are the services' own migration records in schemas whose
// other tables are copied.
var seedSkipTables = []string{"auth.schema_migrations", "storage.migrations"}

// seedRequest is the file `hydra db reseed` leaves for the next start: the
// stack of this branch is destroyed and made again from main's data.
func seedRequest(root string) string {
	return filepath.Join(root, ".process-compose", "supabase-reseed")
}

// seedDone holds the line that says what the last copy did, for `hydra db
// reseed` to print.
func seedDone(root string) string {
	return filepath.Join(root, ".process-compose", "supabase-seeded")
}

// markSeed runs before the stack file's start, with seed_from_main: when
// this start creates the stack (or `hydra db reseed` asked for a new one),
// it leaves render.SeedPending, which makes the start skip migrations and
// seeds and the step after it copy main's data.
func markSeed(root, branch string, run func(...string) ([]byte, error)) error {
	pending := filepath.Join(root, render.SeedPending)
	if _, err := os.Stat(seedRequest(root)); err == nil {
		stacks, err := supabaseStacks(run, root)
		if err != nil {
			return err
		}
		for _, s := range stacks {
			if s.Branch != branch {
				continue
			}
			if _, err := run("stack", "destroy", "--stack-id", s.ID, "--yes"); err != nil {
				return err
			}
			fmt.Printf("hydra: removed this worktree's database for %s, as `hydra db reseed` asked; the new one gets main's data\n", strings.TrimPrefix(branch, "refs/heads/"))
		}
		if err := writeFile(pending, nil); err != nil {
			return err
		}
		return os.Remove(seedRequest(root))
	}
	if _, err := os.Stat(pending); err == nil {
		return nil // a start that didn't get as far as the copy
	}
	stacks, err := supabaseStacks(run, root)
	if err != nil {
		return err
	}
	for _, s := range stacks {
		if s.Branch == branch {
			return nil // resumed with its data
		}
	}
	return writeFile(pending, nil)
}

// seedFromMain gives the worktree's new, empty database main's schema and
// rows (see above). mainURL reaches main's as supabase_admin; wtURL the
// worktree's as postgres, who owns what migrations create.
func seedFromMain(root, mainURL, wtURL string) error {
	began := time.Now()
	ctx := context.Background()
	src, err := connect(ctx, mainURL)
	if err != nil {
		// One short line: hydra status shows it cut at 160 characters.
		return fmt.Errorf("main's database (%s) isn't answering (%s), and this worktree's starts from its data: start main's stack first",
			hostPort(mainURL), briefErr(err))
	}
	defer src.Close(ctx)
	dst, err := connect(ctx, adminURL(wtURL))
	if err != nil {
		return fmt.Errorf("this worktree's database at %s: %s", hostPort(wtURL), briefErr(err))
	}
	defer dst.Close(ctx)
	// A stack file that sets main's ports in the command every copy runs
	// starts the worktree's stack on them: both addresses then reach one
	// server, which the copy would empty.
	if same, err := sameServer(ctx, src, dst); err != nil || same {
		if err == nil {
			err = fmt.Errorf("main's database and this worktree's are one server (%s): its stack runs on main's ports; set those in baseline_command only", hostPort(wtURL))
		}
		return err
	}

	files, err := migrationFiles(filepath.Join(root, "supabase", "migrations"))
	if err != nil {
		return err
	}
	have, err := appliedVersions(ctx, src)
	if err != nil {
		return fmt.Errorf("main's database: %w", err)
	}
	here, err := appliedVersions(ctx, dst)
	if err != nil {
		return fmt.Errorf("this worktree's database: %w", err)
	}
	mains, err := mainsMigrations(have, here, files)
	if err != nil {
		return err
	}
	if err := applyMigrations(root, mains, wtURL); err != nil {
		return err
	}
	st, err := copyData(ctx, src, dst)
	if err != nil {
		return fmt.Errorf("copying main's data: %w", err)
	}
	plural := "s"
	if len(mains) == 1 {
		plural = ""
	}
	line := fmt.Sprintf("copied main's data into this worktree's database: %d rows in %d tables (%s), on main's schema (%d migration%s), in %s",
		st.rows, st.tables, size(st.bytes), len(mains), plural, time.Since(began).Round(100*time.Millisecond))
	fmt.Println("hydra: " + line)
	if len(st.missing) > 0 {
		fmt.Printf("hydra: left out, as this worktree's schema at main's migrations doesn't have them (made by hand in main?): %s\n", strings.Join(st.missing, ", "))
	}
	return writeFile(seedDone(root), []byte(line+"\n"))
}

func connect(ctx context.Context, u string) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(u)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["application_name"] = "hydra seed_from_main"
	return pgx.ConnectConfig(ctx, cfg)
}

// sameServer reports whether two connections reach one Postgres server: its
// data directory's id, set when it was made, is the same.
func sameServer(ctx context.Context, a, b *pgx.Conn) (bool, error) {
	var ia, ib int64
	q := "select system_identifier from pg_control_system()"
	if err := a.QueryRow(ctx, q).Scan(&ia); err != nil {
		return false, err
	}
	if err := b.QueryRow(ctx, q).Scan(&ib); err != nil {
		return false, err
	}
	return ia == ib, nil
}

// briefErr is the end of a connection error, on one line: pgx names every
// address it tried, each on a line of its own, and says the same each time.
func briefErr(err error) string {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if i := strings.LastIndex(last, ": "); i >= 0 && len(lines) > 1 {
		last = last[i+2:]
	}
	return last
}

func hostPort(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return u
}

var migrationName = regexp.MustCompile(`^([0-9]+)_.*\.sql$`)

// migrationFiles maps each migration's version to its file name, as the
// Supabase CLI reads supabase/migrations.
func migrationFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if m := migrationName.FindStringSubmatch(e.Name()); m != nil && !e.IsDir() {
			out[m[1]] = e.Name()
		}
	}
	return out, nil
}

// appliedVersions are the migrations a database has, by the Supabase CLI's
// record of them; none when it has no record yet.
func appliedVersions(ctx context.Context, c *pgx.Conn) ([]string, error) {
	var exists bool
	if err := c.QueryRow(ctx, `select to_regclass('supabase_migrations.schema_migrations') is not null`).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	rows, err := c.Query(ctx, `select version from supabase_migrations.schema_migrations order by version`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// mainsMigrations are the files to apply to bring the worktree's database to
// main's migrations: main has applied them, and the worktree's database has
// none main hasn't. It fails when main has one this branch lacks (the branch
// is behind main), and when the worktree's database is past main's already.
func mainsMigrations(mains, here []string, files map[string]string) ([]string, error) {
	inMain := map[string]bool{}
	var missing, out []string
	for _, v := range mains {
		inMain[v] = true
		if f, ok := files[v]; ok {
			out = append(out, f)
		} else {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("main's database has migrations this branch doesn't (%s), so its data doesn't fit the branch's schema. "+
			"Merge main into this branch (hydra update --here), then hydra db reseed", strings.Join(missing, ", "))
	}
	for _, v := range here {
		if !inMain[v] {
			return nil, fmt.Errorf("this worktree's database already has migration %s, which main's hasn't, so main's data can't be copied under it; hydra db reseed makes it again", v)
		}
	}
	sort.Strings(out)
	return out, nil
}

// applyMigrations runs the given migration files on the worktree's database
// with the Supabase CLI, from a directory that holds only those: the CLI
// applies every pending file it sees.
func applyMigrations(root string, names []string, dbURL string) error {
	if len(names) == 0 {
		return nil
	}
	dir, err := os.MkdirTemp("", "hydra-seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	mig := filepath.Join(dir, "supabase", "migrations")
	if err := os.MkdirAll(mig, 0o755); err != nil {
		return err
	}
	for _, n := range names {
		if err := os.Symlink(filepath.Join(root, "supabase", "migrations", n), filepath.Join(mig, n)); err != nil {
			return err
		}
	}
	// No config.toml there, so the CLI doesn't know the address is local
	// and would ask for TLS.
	cmd := exec.Command("supabase", "migration", "up", "--include-all", "--workdir", dir, "--db-url", dbURL+"?sslmode=disable")
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("applying main's migrations to this worktree's database (before main's data goes in): %w", err)
	}
	return nil
}

type seedStats struct {
	tables, rows int
	bytes        int64
	missing      []string
}

type seedTable struct {
	name string // quoted, schema.table
	cols []string
}

// copyData copies main's rows into the worktree's database, which has main's
// schema: in one transaction on each side, so the copy is of one moment of
// main's, and the worktree's database gets all of it or none. Triggers and
// foreign keys are off while it goes in (session_replication_role), as in
// a pg_dump restore: the rows were consistent in main.
func copyData(ctx context.Context, src, dst *pgx.Conn) (seedStats, error) {
	var st seedStats
	stx, err := src.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return st, err
	}
	defer stx.Rollback(ctx)
	tables, err := seedTables(ctx, stx)
	if err != nil {
		return st, err
	}
	dtx, err := dst.Begin(ctx)
	if err != nil {
		return st, err
	}
	defer dtx.Rollback(ctx)
	if _, err := dtx.Exec(ctx, "set local session_replication_role = replica"); err != nil {
		return st, err
	}
	var present []seedTable
	for _, t := range tables {
		var ok bool
		if err := dtx.QueryRow(ctx, "select to_regclass($1) is not null", t.name).Scan(&ok); err != nil {
			return st, err
		}
		if ok {
			present = append(present, t)
		} else {
			st.missing = append(st.missing, t.name)
		}
	}
	if len(present) > 0 {
		// Rows the migrations inserted (lookup tables, say) are main's too.
		names := make([]string, len(present))
		for i, t := range present {
			names[i] = t.name
		}
		if _, err := dtx.Exec(ctx, "truncate "+strings.Join(names, ", ")); err != nil {
			return st, err
		}
	}
	for _, t := range present {
		n, b, err := copyTable(ctx, stx.Conn(), dtx.Conn(), t)
		if err != nil {
			return st, fmt.Errorf("%s: %w", t.name, err)
		}
		st.tables++
		st.rows += int(n)
		st.bytes += b
	}
	if err := copySequences(ctx, stx, dtx); err != nil {
		return st, err
	}
	return st, dtx.Commit(ctx)
}

// seedTables lists main's tables whose rows are copied, each with the
// columns COPY can write (not generated ones). Partitioned tables are copied
// through their partitions, and tables an extension owns are the
// extension's.
func seedTables(ctx context.Context, tx pgx.Tx) ([]seedTable, error) {
	rows, err := tx.Query(ctx, `
select format('%I.%I', n.nspname, c.relname), n.nspname, c.relname,
       array(select quote_ident(a.attname) from pg_attribute a
             where a.attrelid = c.oid and a.attnum > 0 and not a.attisdropped and a.attgenerated = ''
             order by a.attnum)
from pg_class c join pg_namespace n on n.oid = c.relnamespace
where c.relkind = 'r' and c.relpersistence = 'p'
  and n.nspname <> all($1) and n.nspname not like 'pg\_%'
  and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass and d.objid = c.oid and d.deptype = 'e')
order by 1`, seedSkipSchemas)
	if err != nil {
		return nil, err
	}
	var out []seedTable
	for rows.Next() {
		var t seedTable
		var schema, name string
		if err := rows.Scan(&t.name, &schema, &name, &t.cols); err != nil {
			return nil, err
		}
		if !skipTable(schema + "." + name) {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

func skipTable(name string) bool {
	for _, s := range seedSkipTables {
		if s == name {
			return true
		}
	}
	return false
}

// copyTable streams one table from main into the worktree's database in
// COPY's text format, which, unlike the binary one, doesn't carry type ids
// that differ between the two servers.
func copyTable(ctx context.Context, src, dst *pgx.Conn, t seedTable) (int64, int64, error) {
	cols := "(" + strings.Join(t.cols, ", ") + ")"
	pr, pw := io.Pipe()
	// CopyTo writes row by row; a pipe hands each write over on its own.
	buf := bufio.NewWriterSize(pw, 1<<20)
	counted := &countWriter{w: buf}
	done := make(chan error, 1)
	go func() {
		_, err := src.PgConn().CopyTo(ctx, counted, "copy "+t.name+" "+cols+" to stdout")
		if err == nil {
			err = buf.Flush()
		}
		pw.CloseWithError(err)
		done <- err
	}()
	tag, err := dst.PgConn().CopyFrom(ctx, pr, "copy "+t.name+" "+cols+" from stdin")
	pr.CloseWithError(err) // lets the reading side end if this one failed first
	if rerr := <-done; err == nil && rerr != nil {
		err = rerr
	}
	if err != nil {
		return 0, 0, err
	}
	return tag.RowsAffected(), counted.n, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// copySequences sets each sequence of the copied schemas to where main's
// is, so the next id after main's rows is a new one.
func copySequences(ctx context.Context, src, dst pgx.Tx) error {
	rows, err := src.Query(ctx, `
select format('%I.%I', schemaname, sequencename), last_value, start_value
from pg_sequences s
where schemaname <> all($1) and schemaname not like 'pg\_%'
  and not exists (select 1 from pg_depend d where d.classid = 'pg_class'::regclass
                  and d.objid = format('%I.%I', schemaname, sequencename)::regclass and d.deptype = 'e')`, seedSkipSchemas)
	if err != nil {
		return err
	}
	type seq struct {
		name        string
		last, start *int64
	}
	seqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (seq, error) {
		var s seq
		return s, r.Scan(&s.name, &s.last, &s.start)
	})
	if err != nil {
		return err
	}
	for _, s := range seqs {
		var ok bool
		if err := dst.QueryRow(ctx, "select to_regclass($1) is not null", s.name).Scan(&ok); err != nil || !ok {
			if err != nil {
				return err
			}
			continue
		}
		// Never used in main: back to its start, unused.
		value, called := s.start, false
		if s.last != nil {
			value, called = s.last, true
		}
		if value == nil {
			continue
		}
		if _, err := dst.Exec(ctx, "select setval($1::regclass, $2, $3)", s.name, *value, called); err != nil {
			return fmt.Errorf("sequence %s: %w", s.name, err)
		}
	}
	return nil
}

// size is a byte count for a person: 312 kB, 4.1 MB.
func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d kB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

// dbCmd is `hydra db reseed`: the worktree's own Supabase is made again
// from main's data. Its process is restarted with a request for that, and
// the command waits for the database to be ready, or for the start to fail.
func dbCmd(args []string) error {
	if len(args) != 1 || args[0] != "reseed" {
		return fmt.Errorf("usage: hydra db reseed   (in a worktree: make its own database again from main's data)")
	}
	t, err := resolve("", "")
	if err != nil {
		return err
	}
	co := t.checkout
	if co.IsMain() {
		return fmt.Errorf("hydra db reseed is for a worktree's own database; main's is where the data comes from")
	}
	name, err := seededService(t.stack)
	if err != nil {
		return err
	}
	c := pc.Unix(socketFor(co.Root))
	if !c.Alive() {
		return fmt.Errorf("this worktree's stack isn't up: hydra up -D starts it, with main's data in a new database")
	}
	if _, err := c.Process(name); err != nil {
		return fmt.Errorf("this worktree doesn't run its own %s (its branch didn't change the schema), so it uses main's database already", name)
	}
	os.Remove(seedDone(co.Root))
	if err := writeFile(seedRequest(co.Root), nil); err != nil {
		return err
	}
	// The marker goes now, not when the restart gets to it: what follows
	// waits for the new one.
	os.Remove(supabaseMarker(co.Root))
	fmt.Printf("hydra: restarting %s: its database here is removed and made again from main's data\n", name)
	if err := c.Restart(name); err != nil {
		return err
	}
	return waitSeeded(c, co, name)
}

// seededService is the Supabase service with seed_from_main.
func seededService(st *config.Stack) (string, error) {
	for _, n := range st.Names() {
		if s := st.Services[n]; s.Kind == config.KindSupabase && s.SeedFromMain {
			return n, nil
		}
	}
	return "", fmt.Errorf("no kind: supabase service has x-hydra.seed_from_main: true, so a worktree's database starts from the seed files (to rebuild it from them: supabase db reset at the worktree's root)")
}

func waitSeeded(c *pc.Client, co gitx.Checkout, name string) error {
	began := time.Now()
	for ; ; time.Sleep(500 * time.Millisecond) {
		if _, err := os.Stat(supabaseMarker(co.Root)); err == nil {
			if line, err := os.ReadFile(seedDone(co.Root)); err == nil {
				fmt.Print("hydra: " + string(line))
			}
			fmt.Printf("hydra: %s is ready, with main's data and this branch's migrations (%s)\n", name, time.Since(began).Round(time.Second))
			return nil
		}
		p, err := c.Process(name)
		if err != nil {
			return err
		}
		if p.Health() == pc.Failed && time.Since(began) > 2*time.Second {
			logs, _ := c.Logs(name, 40)
			var said []string
			for _, l := range logs {
				if !strings.HasPrefix(l, "A new version of Supabase CLI") && !strings.HasPrefix(l, "We recommend updating") && strings.TrimSpace(l) != "" {
					said = append(said, l)
				}
			}
			if len(said) > 12 {
				said = said[len(said)-12:]
			}
			return fmt.Errorf("%s failed to start again (%s). Its last output:\n    | %s", name, p.Describe(), strings.Join(said, "\n    | "))
		}
		if time.Since(began) > 15*time.Minute {
			return fmt.Errorf("%s isn't ready after 15 minutes: hydra process logs %s", name, name)
		}
	}
}
