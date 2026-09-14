package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pacenote-sim/server/internal/db/gen"
)

// AcquireTimeout bounds how long a caller waits for a connection from the pool
// before giving up. It is separate from the statement's own
// deadline: waiting five seconds for a free connection and then being given the
// caller's full time to run is the behaviour a busy server wants.
const AcquireTimeout = 5 * time.Second

// StatementTimeout is set on every connection, which is where D-4's
// "statement_timeout on the role" is actually implemented. Setting it as a
// runtime parameter rather than with ALTER ROLE means it applies whether or not
// the operator's database user may alter itself, and it cannot be forgotten
// when someone creates a second user.
const StatementTimeout = 30 * time.Second

// IdleInTransactionTimeout kills a transaction that was opened and then
// abandoned, which is what a crashed request looks like from the database's
// side. Without it one such request holds its locks until the process restarts.
const IdleInTransactionTimeout = 60 * time.Second

// MinConns is the floor on pool size, so a single-core machine still has room
// for a request, a background job and a health check at once.
const MinConns = 4

// Store is the server's database. It owns a pgx pool and exposes typed methods;
// the generated sqlc queries are reached through it and not directly, so that
// every call site goes through one place that knows about timeouts.
type Store struct {
	pool *pgxpool.Pool
	q    *gen.Queries
	log  *slog.Logger
}

// Open connects to the database at url and returns a ready [Store]. It does not
// run migrations: [Store.Migrate] does, and setup decides when.
//
// The pool is sized at four connections per core, per D-4, with a floor of
// [MinConns].
func Open(ctx context.Context, url string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	cfg, err := poolConfig(url)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, classify(err, cfg.ConnConfig)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, classify(err, cfg.ConnConfig)
	}
	s := &Store{pool: pool, log: log}
	s.q = gen.New(&timedPool{pool: pool, acquire: AcquireTimeout})
	return s, nil
}

// Close returns every connection to the operating system. It blocks until the
// connections in use have been released.
func (s *Store) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
}

// Ping reports whether the database is answering.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return classify(err, s.pool.Config().ConnConfig)
	}
	return nil
}

// Queries is the generated query set, bound to this pool. It exists for the
// packages inside internal/db that need it; everything outside calls the typed
// methods on [Store].
func (s *Store) Queries() *gen.Queries { return s.q }

// PoolStats is what the pool is doing right now, for the metrics port.
func (s *Store) PoolStats() (acquired, idle, total int32) {
	st := s.pool.Stat()
	return st.AcquiredConns(), st.IdleConns(), st.TotalConns()
}

func poolConfig(url string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, &ConnectError{
			Reason:  ReasonBadConnectionString,
			Message: "That is not a PostgreSQL connection string. It looks like \"" + exampleDSN + "\".",
			err:     err,
		}
	}
	maxConns := int32(4 * runtime.NumCPU()) //nolint:gosec // NumCPU is small and positive; the conversion cannot overflow.
	if maxConns < MinConns {
		maxConns = MinConns
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	if cfg.ConnConfig.ConnectTimeout == 0 {
		cfg.ConnConfig.ConnectTimeout = AcquireTimeout
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	setIfAbsent(cfg.ConnConfig.RuntimeParams, "application_name", "pacenote-server")
	setIfAbsent(cfg.ConnConfig.RuntimeParams, "statement_timeout", millis(StatementTimeout))
	setIfAbsent(cfg.ConnConfig.RuntimeParams, "idle_in_transaction_session_timeout", millis(IdleInTransactionTimeout))
	return cfg, nil
}

func setIfAbsent(m map[string]string, k, v string) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

func millis(d time.Duration) string { return fmt.Sprintf("%d", d.Milliseconds()) }

// timedPool bounds the wait for a connection at [AcquireTimeout] without
// bounding the statement that runs on it. pgxpool has no such setting, so the
// acquisition is done here and the connection released when the caller is done
// with it — which for a query means when the rows are closed, not when Query
// returns.
type timedPool struct {
	pool    *pgxpool.Pool
	acquire time.Duration
}

func (t *timedPool) conn(ctx context.Context) (*pgxpool.Conn, error) {
	actx, cancel := context.WithTimeout(ctx, t.acquire)
	defer cancel()
	c, err := t.pool.Acquire(actx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf("db: no database connection became free within %s: %w", t.acquire, err)
		}
		return nil, fmt.Errorf("db: cannot take a database connection: %w", err)
	}
	return c, nil
}

func (t *timedPool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	c, err := t.conn(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer c.Release()
	tag, err := c.Exec(ctx, sql, args...)
	if err != nil {
		return tag, fmt.Errorf("db: %w", err)
	}
	return tag, nil
}

func (t *timedPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c, err := t.conn(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		c.Release()
		return nil, fmt.Errorf("db: %w", err)
	}
	return &releasingRows{Rows: rows, conn: c}, nil
}

func (t *timedPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c, err := t.conn(ctx)
	if err != nil {
		return errRow{err: err}
	}
	return &releasingRow{row: c.QueryRow(ctx, sql, args...), conn: c}
}

// releasingRows hands the connection back when the rows are closed, which is
// the contract pgxpool.Pool.Query has and which sqlc's generated code relies on.
type releasingRows struct {
	pgx.Rows
	conn     *pgxpool.Conn
	released bool
}

func (r *releasingRows) Close() {
	r.Rows.Close()
	if !r.released {
		r.released = true
		r.conn.Release()
	}
}

// releasingRow hands the connection back after Scan, which is the only thing
// that can be done with a pgx.Row.
type releasingRow struct {
	row  pgx.Row
	conn *pgxpool.Conn
}

func (r *releasingRow) Scan(dst ...any) error {
	defer r.conn.Release()
	if err := r.row.Scan(dst...); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	return nil
}

// errRow carries an acquisition failure to the Scan that was going to happen
// anyway, so a caller sees one error instead of a nil pointer.
type errRow struct{ err error }

func (e errRow) Scan(...any) error { return e.err }
