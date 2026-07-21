package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const SchemaVersion = 11

var (
	ErrDatabaseNotFound = errors.New("SQLite database not found")
	ErrFutureSchema     = errors.New("SQLite schema is newer than this local-agent version")
	ErrStateResetNeeded = errors.New("SQLite schema requires state reset")
	ErrMetadataConflict = errors.New("conversation metadata conflicts with the canonical key")
)

type FutureSchemaError struct {
	Found     int
	Supported int
}

func (e *FutureSchemaError) Error() string {
	return fmt.Sprintf("%v: found version %d, supported version %d", ErrFutureSchema, e.Found, e.Supported)
}

func (e *FutureSchemaError) Unwrap() error { return ErrFutureSchema }

type StateResetNeededError struct {
	Found     int
	Supported int
}

func (e *StateResetNeededError) Error() string {
	return fmt.Sprintf("%v: found version %d, requires fresh version %d state", ErrStateResetNeeded, e.Found, e.Supported)
}

func (e *StateResetNeededError) Unwrap() error { return ErrStateResetNeeded }

type Store struct {
	db *sql.DB
}

// OpenExisting opens and migrates an existing database. SQLite's mode=rw is
// intentional: run must never create a missing database as a side effect.
func OpenExisting(ctx context.Context, path string) (*Store, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrDatabaseNotFound, path)
		}
		return nil, fmt.Errorf("stat SQLite database %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("SQLite database path %q is a directory", path)
	}

	return open(ctx, path, "rw")
}

// Create creates a new database file with restrictive permissions and applies
// all known migrations. It fails rather than opening an existing file.
func Create(ctx context.Context, path string) (*Store, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create SQLite database %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close new SQLite database %q: %w", path, err)
	}

	store, err := OpenExisting(ctx, path)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return store, nil
}

// Initialize is the idempotent bootstrap entry point. The parent directory is
// an artifact owned by the bootstrap/filesystem layer and must already exist.
func Initialize(ctx context.Context, path string) (*Store, error) {
	store, err := OpenExisting(ctx, path)
	if err == nil {
		return store, nil
	}
	if !errors.Is(err, ErrDatabaseNotFound) {
		return nil, err
	}

	store, err = Create(ctx, path)
	if err == nil {
		return store, nil
	}
	// Another initializer may have won the O_EXCL race.
	if errors.Is(err, os.ErrExist) {
		return OpenExisting(ctx, path)
	}
	return nil, err
}

func open(ctx context.Context, path, mode string) (*Store, error) {
	dsn, err := dataSourceName(path, mode)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database %q: %w", path, err)
	}
	// A single connection keeps connection-scoped pragmas deterministic and is
	// sufficient for this single-process, write-light local application.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open SQLite database %q: %w", path, err)
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate SQLite database %q: %w", path, err)
	}
	return &Store{db: db}, nil
}

func dataSourceName(path, mode string) (string, error) {
	if path == "" {
		return "", errors.New("SQLite database path is required")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve SQLite database path %q: %w", path, err)
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}
	query := u.Query()
	query.Set("mode", mode)
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
