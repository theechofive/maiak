package migrations_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
	"github.com/theechofive/maiak/backend/migrations"
)

// rollbackSchemaVersionGuard undoes the O-06 migration, so a test can replay an older migration
// (goose refuses to apply a missing older version).
var rollbackSchemaVersionGuard = []string{
	`REVOKE SELECT ON goose_db_version FROM maiak_api, maiak_worker`,
	`DELETE FROM goose_db_version WHERE version_id = 20261109090000`,
}

// TestIntegrationSchemaVersionGuard checks the start-up guard of api, worker and jobs (O-06): it
// passes on the current schema and refuses a schema with a migration the binary does not know.
func TestIntegrationSchemaVersionGuard(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("schemaguard"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	pools := map[string]string{"api": env.APIURL, "worker": env.WorkerURL}

	for name, url := range pools {
		if err := migrations.CheckNotAhead(ctx, env.Pool(t, url, 1)); err != nil {
			t.Fatalf("%s on the current schema: %v", name, err)
		}
	}

	latest, err := migrations.Latest()
	if err != nil {
		t.Fatal(err)
	}
	ahead := latest + 1
	pgtest.MustExec(t, admin, fmt.Sprintf(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (%d, true)`, ahead))
	t.Cleanup(func() {
		pgtest.MustExec(t, admin, fmt.Sprintf(`DELETE FROM goose_db_version WHERE version_id = %d`, ahead))
	})
	for name, url := range pools {
		err := migrations.CheckNotAhead(ctx, env.Pool(t, url, 1))
		if !errors.Is(err, migrations.ErrSchemaAhead) || !strings.Contains(err.Error(), fmt.Sprint(ahead)) {
			t.Fatalf("%s on a newer schema: %v, want ErrSchemaAhead naming %d", name, err, ahead)
		}
	}
}
