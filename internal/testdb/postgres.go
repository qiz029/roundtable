package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func NewPostgresDatabase(ctx context.Context, baseURL string) (string, func(context.Context) error, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse postgres url: %w", err)
	}

	dbName, err := testDatabaseName()
	if err != nil {
		return "", nil, err
	}
	adminURL := *parsed
	adminURL.Path = "/postgres"
	testURL := *parsed
	testURL.Path = "/" + dbName

	adminDB, err := sql.Open("pgx", adminURL.String())
	if err != nil {
		return "", nil, fmt.Errorf("open postgres admin connection: %w", err)
	}
	if err := adminDB.PingContext(ctx); err != nil {
		_ = adminDB.Close()
		return "", nil, fmt.Errorf("ping postgres admin connection: %w", err)
	}
	if _, err := adminDB.ExecContext(ctx, `CREATE DATABASE `+quoteIdentifier(dbName)); err != nil {
		_ = adminDB.Close()
		return "", nil, fmt.Errorf("create test database: %w", err)
	}

	cleanup := func(ctx context.Context) error {
		defer adminDB.Close()
		_, err := adminDB.ExecContext(ctx, `DROP DATABASE IF EXISTS `+quoteIdentifier(dbName)+` WITH (FORCE)`)
		return err
	}
	return testURL.String(), cleanup, nil
}

func testDatabaseName() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return "roundtable_test_" + hex.EncodeToString(raw), nil
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}
