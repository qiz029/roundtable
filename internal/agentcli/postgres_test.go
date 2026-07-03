package agentcli_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/qiz029/roundtable/internal/roundtable"
	"github.com/qiz029/roundtable/internal/testdb"
)

func newTestApp(t *testing.T, opts roundtable.Options) (*roundtable.App, error) {
	t.Helper()
	opts.DatabaseURL = newTestDatabaseURL(t)
	return roundtable.NewApp(opts)
}

func newTestDatabaseURL(t *testing.T) string {
	t.Helper()

	baseURL := os.Getenv("ROUNDTABLE_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("set ROUNDTABLE_TEST_DATABASE_URL to run Postgres integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	databaseURL, cleanup, err := testdb.NewPostgresDatabase(ctx, baseURL)
	if err != nil {
		t.Fatalf("create postgres test database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = cleanup(ctx)
	})

	return databaseURL
}
