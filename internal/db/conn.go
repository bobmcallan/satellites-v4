// Package db owns the SurrealDB connection used by satellites-v4 stores.
// The connection bundle resolves WebSocket DSN + signin credentials + ns/db
// selection in one place so stores can remain ignorant of them.
package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/surrealdb/surrealdb.go"
)

// Config bundles the fields required to open a SurrealDB connection. Built
// from the runtime config.DBDSN form `ws://<user>:<pass>@<host>:<port>/<ns>/<db>`.
type Config struct {
	DSN       string
	Namespace string
	Database  string
	Username  string
	Password  string
}

// ParseDSN extracts the five Config fields from a DSN of the form
// `ws://root:root@localhost:8000/rpc/<namespace>/<database>`. Minimal parser
// — accepts short forms (defaults for namespace/database/user/pass) so
// testcontainers and docker-compose DSNs are interchangeable.
func ParseDSN(dsn string) (Config, error) {
	if dsn == "" {
		return Config{}, errors.New("db: empty DSN")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return Config{}, fmt.Errorf("db: parse DSN: %w", err)
	}
	cfg := Config{
		DSN:       (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String(),
		Namespace: "satellites",
		Database:  "satellites",
		Username:  "root",
		Password:  "root",
	}
	if u.User != nil {
		cfg.Username = u.User.Username()
		if p, ok := u.User.Password(); ok {
			cfg.Password = p
		}
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	// Accepts trailing /rpc/<ns>/<db> — index from the end.
	switch len(parts) {
	case 3:
		// rpc/<ns>/<db>
		cfg.Namespace = parts[1]
		cfg.Database = parts[2]
	case 2:
		// <ns>/<db>
		cfg.Namespace = parts[0]
		cfg.Database = parts[1]
	}
	// Strip rpc suffix — surrealdb.go Connect expects the ws root.
	cfg.DSN = strings.TrimSuffix(cfg.DSN, "/"+strings.Join(parts, "/"))
	if !strings.HasSuffix(cfg.DSN, "/rpc") {
		cfg.DSN += "/rpc"
	}
	return cfg, nil
}

// Connect opens a SurrealDB connection, signs in, and selects ns/db. The
// returned *surrealdb.DB is safe for concurrent use (the driver owns
// multiplexing).
func Connect(ctx context.Context, cfg Config) (*surrealdb.DB, error) {
	db, err := surrealdb.FromEndpointURLString(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db: connect: %w", err)
	}
	if _, err := db.SignIn(ctx, &surrealdb.Auth{Username: cfg.Username, Password: cfg.Password}); err != nil {
		return nil, fmt.Errorf("db: signin: %w", err)
	}
	if err := db.Use(ctx, cfg.Namespace, cfg.Database); err != nil {
		return nil, fmt.Errorf("db: use ns/db: %w", err)
	}
	return db, nil
}

// Ping runs a trivial query to confirm the connection is alive — used by
// /healthz to expose db_ok.
func Ping(ctx context.Context, db *surrealdb.DB) error {
	if db == nil {
		return errors.New("db: nil connection")
	}
	if _, err := surrealdb.Query[any](ctx, db, "RETURN 1", nil); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}
