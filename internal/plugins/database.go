package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	// This is the one connection in the server that is not the server's. It is
	// opened as the plugin's own role, with the plugin's own search_path, so
	// that a plugin's migrations land in the plugin's schema and are owned by
	// the plugin — which is the whole point of giving it a role. internal/db
	// cannot do it, because internal/db is the pool connected as the server.
	//nolint:depguard // as above: a connection as somebody other than us.
	"github.com/jackc/pgx/v5"

	"github.com/pacenote-sim/plugin"
	"github.com/pacenote-sim/server/internal/db"
)

// A plugin that asked for tables gets a PostgreSQL role and a schema of its own.
// This file decides when that happens and what it is worth; internal/db runs the
// statements.
//
// The split matters because the two halves fail differently. A statement that is
// refused is the operator's database saying no, and belongs where every other
// statement is. Deciding to generate a new password because the data key changed
// is policy about plugins, and belongs here.

// passwordBytes is the size of a generated role password before hex encoding.
// Thirty-two bytes is the same strength as the data key, and the password is
// never typed by a person, so there is no reason for it to be shorter.
const passwordBytes = 32

// migrationSuffix is what counts as a migration in a plugin's migrations
// directory. Everything else in there is ignored, so a plugin may keep a README
// beside its schema.
const migrationSuffix = ".sql"

// migrationTable is the plugin's own bookkeeping, in the plugin's own schema.
// It is created as the plugin's role, so it belongs to the plugin and leaves
// with it. The name starts with an underscore so it sorts away from a plugin's
// real tables in every tool that lists them.
const migrationTable = "_pacenote_migrations"

// ErrNoDataKey is a plugin asking for a database on a server with no data key.
// There is nothing to seal the role's password with, and storing it in clear to
// get past that would be worse than refusing.
var ErrNoDataKey = errors.New("plugins: the server has no data key, so a plugin database cannot be created")

// provision makes sure a plugin's database exists and returns the connection
// string to hand it. A plugin that declared no database gets an empty string and
// no role, which is most of them.
//
// It is safe to call on every start. The role, the schema and the grants are all
// created idempotently, the password is only regenerated when the stored one
// cannot be read, and migrations already applied are skipped.
func (h *Host) provision(ctx context.Context, m plugin.Manifest, dir string) (dsn, password string, err error) {
	if !m.Capabilities.Database {
		return "", "", nil
	}
	store := h.opts.Store
	key := h.opts.Keyring.Key()
	if key == nil {
		return "", "", fmt.Errorf("%w: %s asked for one", ErrNoDataKey, m.Name)
	}

	// A password that is already stored and still readable is kept. Rotating it
	// on every start would work — the role is altered, not recreated — but it
	// would mean a restart during which the old connection string is wrong,
	// and nothing is gained by it.
	switch row, err := store.PluginDatabase(ctx, m.Name); {
	case err != nil:
		// Not found is normal: the plugin row is written before this runs on a
		// first install. Anything else is the database failing and is fatal.
		if !errors.Is(err, db.ErrNotFound) {
			return "", "", err
		}
	case row.Provisioned && len(row.PasswordSealed) > 0:
		opened, err := key.Open(row.PasswordSealed)
		if err != nil {
			// The data key was regenerated, or the data directory was lost
			// while the database survived. The role is still there and still
			// owns the plugin's tables; only the password is unreachable. A new
			// one is generated and set on the existing role, so the plugin's
			// data is not lost — which is the whole reason the password is
			// stored rather than the role being dropped and remade.
			h.opts.Log.LogAttrs(ctx, slog.LevelWarn,
				"the stored database password for a plugin cannot be read, so a new one is being set",
				slog.String("plugin", m.Name))
		} else {
			password = opened
		}
	}
	if password == "" {
		if password, err = newPassword(); err != nil {
			return "", "", err
		}
	}

	if err := store.ProvisionPluginDatabase(ctx, m.Name, password); err != nil {
		return "", "", err
	}
	dsn, err = store.PluginDSN(m.Name, password)
	if err != nil {
		return "", "", err
	}

	applied, err := applyMigrations(ctx, dsn, filepath.Join(dir, plugin.MigrationsDir))
	if err != nil {
		return "", "", fmt.Errorf("plugins: migrating %s: %w", m.Name, err)
	}

	sealed, err := key.Seal(password)
	if err != nil {
		return "", "", fmt.Errorf("plugins: sealing the database password of %s: %w", m.Name, err)
	}
	// Recorded last. A crash before this leaves a role that exists and a server
	// that does not know it, which the next start fixes by provisioning again —
	// the safe direction. The other order would leave a server holding a
	// password for a role that was never made.
	if err := store.SetPluginProvisioned(ctx, m.Name, sealed, applied); err != nil {
		return "", "", err
	}
	return dsn, password, nil
}

// deprovision removes a plugin's database and forgets the password that reached
// it. It is what uninstalling means for a plugin that had tables.
func (h *Host) deprovision(ctx context.Context, name string) error {
	store := h.opts.Store
	if err := store.DropPluginDatabase(ctx, name); err != nil {
		return err
	}
	return store.ClearPluginDatabase(ctx, name)
}

// newPassword is a role password nobody will ever type. Hex rather than base64
// so that it carries nothing that has to be escaped in a connection string or a
// statement — the escaping is correct either way, and a credential is a poor
// place to rely on being right about escaping.
func newPassword() (string, error) {
	b := make([]byte, passwordBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("plugins: no randomness available for a database password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// applyMigrations runs a plugin's schema against its own database, as its own
// role, and returns the name of the last file applied.
//
// Every file in the directory ending .sql is a migration, applied in the order
// the names sort, once each. There are no down migrations: uninstalling drops
// the schema whole, which is the only rollback that is reliably correct.
//
// Each file runs inside a transaction with the row recording it, so a migration
// that fails halfway leaves nothing behind and will be tried again. A migration
// that cannot run in a transaction — CREATE INDEX CONCURRENTLY is the usual one
// — is not supported, and failing loudly is better than the alternative.
func applyMigrations(ctx context.Context, dsn, dir string) (string, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", nil
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("connecting as the plugin's own role: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// Unqualified on purpose: the role's search_path puts its own schema first,
	// so this lands in the plugin's schema and is dropped with it.
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+migrationTable+` (
		name       text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return "", fmt.Errorf("creating the migration table: %w", err)
	}

	done := map[string]bool{}
	rows, err := conn.Query(ctx, `SELECT name FROM `+migrationTable)
	if err != nil {
		return "", fmt.Errorf("reading which migrations have run: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return "", err
		}
		done[name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	last := ""
	for _, f := range files {
		if done[f] {
			last = f
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, f)) //nolint:gosec // G304: the plugin directory is the operator's own.
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", f, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return "", fmt.Errorf("%s: %w", f, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+migrationTable+` (name) VALUES ($1)`, f); err != nil {
			_ = tx.Rollback(ctx)
			return "", fmt.Errorf("recording %s: %w", f, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf("committing %s: %w", f, err)
		}
		last = f
	}
	return last, nil
}

// migrationFiles is the .sql files in a directory, sorted by name. A directory
// that is not there is not an error: a plugin may declare a database and keep
// its schema entirely in code.
func migrationFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), migrationSuffix) {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}
