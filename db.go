package plugin

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // the driver: pure Go, so a plugin builds anywhere with no C compiler
)

// dbFile is the plugin's database, in its data folder.
const dbFile = "plugin.db"

// DB is the plugin's own database: a SQLite file in its data folder,
// opened as one process that reads a lot and writes in bursts wants it
// (write-ahead logging, a busy timeout, foreign keys on).
//
// The .sql files in the plugin's migrations folder have been applied, in
// name order, each once, before any handler runs. A plugin with no
// migrations gets an empty database the first time it asks.
//
// It's there once the plugin is running: call it in a handler, not while
// declaring.
func (p *Plugin) DB() *sql.DB {
	db, err := p.openDB()
	if err != nil {
		// Handlers don't run when the database couldn't be brought
		// forward, so this is a call made too early, or a disk gone.
		panic("plugin: DB: " + err.Error())
	}
	return db
}

// hasMigrations is whether the plugin's files hold any migrations.
func (p *Plugin) hasMigrations() bool {
	names, _ := p.migrations()
	return len(names) > 0
}

// migrations are the plugin's .sql files, in the order they're applied.
func (p *Plugin) migrations() ([]string, error) {
	if p.files == nil {
		return nil, nil
	}
	entries, err := fs.ReadDir(p.files, "migrations")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// openDB opens the database and brings it forward, once.
func (p *Plugin) openDB() (*sql.DB, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db != nil {
		return p.db, nil
	}
	if p.broken != nil {
		return nil, p.broken
	}
	data := p.info.Plugin.Data
	if data == "" {
		return nil, errors.New("the database is there once the plugin is running: ask for it in a handler")
	}
	if err := os.MkdirAll(data, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(data, dbFile)
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if err := p.migrate(db, path); err != nil {
		db.Close() //nolint:errcheck // already failing
		return nil, err
	}
	p.db = db
	return db, nil
}

// migrate applies the migrations the database hasn't had, in name order,
// each in a transaction of its own. The file is copied aside first
// (plugin.db.bak), and the copy kept until the next time there's
// something to apply.
func (p *Plugin) migrate(db *sql.DB, path string) error {
	// A migration the plugin stops in is rolled back, as one that fails is.
	ctx := p.ctx
	names, err := p.migrations()
	if err != nil {
		return fmt.Errorf("reading migrations: %w", err)
	}
	if _, err := db.ExecContext(ctx, `create table if not exists _migrations (name text primary key, applied_at integer not null)`); err != nil {
		return fmt.Errorf("opening %s: %w", dbFile, err)
	}
	applied := map[string]bool{}
	rows, err := db.QueryContext(ctx, `select name from _migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close() //nolint:errcheck // already failing
			return err
		}
		applied[name] = true
	}
	// Read short, applied would leave out migrations the database has, and
	// they'd be applied again.
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck // already failing
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var pending []string
	for _, name := range names {
		if !applied[name] {
			pending = append(pending, name)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	if len(applied) > 0 {
		// There's something to lose: keep a copy of it.
		if _, err := db.ExecContext(ctx, `pragma wal_checkpoint(truncate)`); err != nil {
			return fmt.Errorf("copying %s aside: %w", dbFile, err)
		}
		if err := copyFile(path, path+".bak"); err != nil {
			return fmt.Errorf("copying %s aside: %w", dbFile, err)
		}
	}
	for _, name := range pending {
		script, err := fs.ReadFile(p.files, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			tx.Rollback() //nolint:errcheck // already failing
			return fmt.Errorf("migration %s couldn't be applied, so the plugin's database is as it was: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `insert into _migrations (name, applied_at) values (?, ?)`, name, time.Now().Unix()); err != nil {
			tx.Rollback() //nolint:errcheck // already failing
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // already failing
		return err
	}
	return out.Close()
}
