package dbbootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
)

// Role names. They are fixed: grants in the migrations refer to them.
const (
	OwnerRole  = "maiak_owner"
	APIRole    = "maiak_api"
	WorkerRole = "maiak_worker"
)

// Secret keys (see the package documentation).
const (
	KeyOwnerPassword  = "owner_password"
	KeyAPIPassword    = "api_password"
	KeyWorkerPassword = "worker_password"
	KeyOwnerURL       = "owner_url"
	KeyAPIURL         = "api_url"
	KeyWorkerURL      = "worker_url"
)

// lockKey identifies the cluster-wide advisory lock (taken in database "postgres") that
// serialises concurrent bootstrap runs.
const lockKey int64 = 0x6d61_6961_6b62_7331 // "maiakbs1"

type roleSpec struct {
	name          string
	connLimit     int
	passwordKey   string
	urlKey        string
	settings      [][2]string
	runtimeAccess bool // gets USAGE on schema public
}

var roles = []roleSpec{
	{name: OwnerRole, connLimit: 3, passwordKey: KeyOwnerPassword, urlKey: KeyOwnerURL,
		settings: [][2]string{{"statement_timeout", "5min"}}},
	{name: APIRole, connLimit: 20, passwordKey: KeyAPIPassword, urlKey: KeyAPIURL, runtimeAccess: true,
		settings: [][2]string{{"statement_timeout", "15s"}, {"idle_in_transaction_session_timeout", "30s"}}},
	{name: WorkerRole, connLimit: 20, passwordKey: KeyWorkerPassword, urlKey: KeyWorkerURL, runtimeAccess: true,
		settings: [][2]string{{"statement_timeout", "30s"}, {"idle_in_transaction_session_timeout", "30s"}}},
}

// Options configure a bootstrap run.
type Options struct {
	// Admin connects as the instance admin to the maintenance database "postgres".
	Admin *pgx.ConnConfig
	// Host and Port are written into the role URLs (normally the same as Admin's).
	Host string
	Port int
	// DBName is the application database (maiak).
	DBName string
	// SSLMode is written into the role URLs.
	SSLMode string
	// Namespace and SecretName locate the Kubernetes Secret.
	Namespace  string
	SecretName string
	Secrets    SecretStore
	// GeneratePassword defaults to GeneratePassword; tests replace it.
	GeneratePassword func() (string, error)
	// Out receives the verification summary (no secrets).
	Out    io.Writer
	Logger *slog.Logger
}

// Result describes what a run did.
type Result struct {
	SecretCreated   bool
	DatabaseCreated bool
	Roles           []RoleInfo
	DatabaseOwner   string
	// AdminOwnerMember reports whether the admin login is still a member of maiak_owner after
	// the temporary grant was revoked (PostgreSQL 16 keeps an implicit ADMIN OPTION grant for a
	// CREATEROLE creator; on PostgreSQL 15 it should be false).
	AdminOwnerMember bool
}

// RoleInfo are the verified attributes of a role.
type RoleInfo struct {
	Name        string
	CanLogin    bool
	Superuser   bool
	CreateDB    bool
	CreateRole  bool
	Replication bool
	BypassRLS   bool
	ConnLimit   int
	Settings    []string
}

// Run performs the bootstrap. See the package documentation for the exact behaviour.
func Run(ctx context.Context, o Options) (*Result, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	log := o.Logger
	res := &Result{}

	conn, err := pgx.ConnectConfig(ctx, o.Admin)
	if err != nil {
		return nil, fmt.Errorf("dbbootstrap: connect as admin: %w", err)
	}
	defer closeConn(conn)

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		return nil, fmt.Errorf("dbbootstrap: advisory lock: %w", err)
	}
	// Session lock: released when the connection closes, also on failure.

	passwords, created, err := o.passwords(ctx)
	if err != nil {
		return nil, err
	}
	res.SecretCreated = created
	log.Info("credentials ready", "secret", o.Namespace+"/"+o.SecretName, "created", created)

	for _, r := range roles {
		if err := ensureRole(ctx, conn, r, passwords[r.passwordKey]); err != nil {
			return nil, err
		}
	}
	log.Info("roles ensured")

	// RDS: only a member of the owner role may create a database owned by it or change the
	// schema owner. The membership is temporary.
	if _, err := conn.Exec(ctx, "GRANT "+ident(OwnerRole)+" TO CURRENT_USER"); err != nil {
		return nil, fmt.Errorf("dbbootstrap: grant %s to admin: %w", OwnerRole, err)
	}
	revoked := false
	revoke := func() error {
		if revoked {
			return nil
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := conn.Exec(rctx, "REVOKE "+ident(OwnerRole)+" FROM CURRENT_USER"); err != nil {
			return fmt.Errorf("dbbootstrap: revoke temporary %s membership (revoke it manually): %w", OwnerRole, err)
		}
		revoked = true
		return nil
	}
	defer func() {
		if err := revoke(); err != nil {
			log.Error("revoke temporary owner membership failed", "error", err)
		}
	}()

	if res.DatabaseCreated, err = ensureDatabase(ctx, conn, o.DBName); err != nil {
		return nil, err
	}
	if err := ensureInDatabase(ctx, o.Admin, o.DBName); err != nil {
		return nil, err
	}
	log.Info("database ensured", "database", o.DBName, "created", res.DatabaseCreated)

	// Revoke before verifying, so the summary shows the admin's real final membership.
	if err := revoke(); err != nil {
		return nil, err
	}
	if err := verify(ctx, conn, o.DBName, res); err != nil {
		return nil, err
	}
	if o.Out != nil {
		printSummary(o.Out, o.DBName, res)
	}
	return res, nil
}

func (o *Options) validate() error {
	switch {
	case o.Admin == nil:
		return errors.New("dbbootstrap: admin connection config is required")
	case o.Host == "" || o.Port == 0 || o.DBName == "" || o.SSLMode == "":
		return errors.New("dbbootstrap: host, port, database name and sslmode are required")
	case o.Namespace == "" || o.SecretName == "":
		return errors.New("dbbootstrap: namespace and secret name are required")
	case o.Secrets == nil:
		return errors.New("dbbootstrap: secret store is required")
	}
	if o.GeneratePassword == nil {
		o.GeneratePassword = GeneratePassword
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return nil
}

// passwords returns the role passwords from the Secret, creating the Secret first if needed.
func (o *Options) passwords(ctx context.Context) (map[string]string, bool, error) {
	data, found, err := o.Secrets.Get(ctx, o.Namespace, o.SecretName)
	if err != nil {
		return nil, false, err
	}
	if found {
		pw, err := passwordsFrom(data)
		if err != nil {
			return nil, false, err
		}
		return pw, false, o.repairURLs(ctx, data, pw)
	}
	data = map[string][]byte{}
	for _, r := range roles {
		pw, err := o.GeneratePassword()
		if err != nil {
			return nil, false, err
		}
		data[r.passwordKey] = []byte(pw)
		data[r.urlKey] = []byte(RoleURL(r.name, pw, o.Host, o.Port, o.DBName, o.SSLMode))
	}
	err = o.Secrets.Create(ctx, o.Namespace, o.SecretName, data)
	if errors.Is(err, ErrSecretExists) {
		// Another run won the race; use what it stored.
		data, found, err = o.Secrets.Get(ctx, o.Namespace, o.SecretName)
		if err != nil {
			return nil, false, err
		}
		if !found {
			return nil, false, errors.New("dbbootstrap: secret reported as existing but cannot be read")
		}
		pw, err := passwordsFrom(data)
		return pw, false, err
	}
	if err != nil {
		return nil, false, err
	}
	pw, err := passwordsFrom(data)
	return pw, true, err
}

// urlDrift returns the *_url keys that are missing or do not match the passwords and the
// connection settings of this run, with their correct values.
func (o *Options) urlDrift(data map[string][]byte, passwords map[string]string) map[string][]byte {
	drift := map[string][]byte{}
	for _, r := range roles {
		want := RoleURL(r.name, passwords[r.passwordKey], o.Host, o.Port, o.DBName, o.SSLMode)
		if string(data[r.urlKey]) != want {
			drift[r.urlKey] = []byte(want)
		}
	}
	return drift
}

// repairURLs rewrites missing or stale *_url keys (for example after a host change or a manual
// password rotation). Passwords are never rewritten. Only key names are logged.
func (o *Options) repairURLs(ctx context.Context, data map[string][]byte, passwords map[string]string) error {
	drift := o.urlDrift(data, passwords)
	if len(drift) == 0 {
		return nil
	}
	keys := make([]string, 0, len(drift))
	for k := range drift {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	o.Logger.Warn("secret connection URLs are missing or stale; rebuilding them", "secret", o.Namespace+"/"+o.SecretName, "keys", keys)
	if err := o.Secrets.Update(ctx, o.Namespace, o.SecretName, drift); err != nil {
		return fmt.Errorf("dbbootstrap: rebuild secret URLs: %w", err)
	}
	return nil
}

func passwordsFrom(data map[string][]byte) (map[string]string, error) {
	out := map[string]string{}
	var missing []string
	for _, r := range roles {
		v := strings.TrimSpace(string(data[r.passwordKey]))
		if v == "" {
			missing = append(missing, r.passwordKey)
			continue
		}
		out[r.passwordKey] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("dbbootstrap: secret exists but lacks %s; refusing to overwrite it (fix or delete the Secret)", strings.Join(missing, ", "))
	}
	return out, nil
}

// ident quotes an identifier.
func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

// createRoleSQL creates the role with all attributes if it does not exist yet. Role names are
// package constants; they are quoted regardless.
func createRoleSQL(r roleSpec) string {
	return fmt.Sprintf(`DO $do$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = %s) THEN
    CREATE ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT %d;
  END IF;
END
$do$`, quoteLiteral(r.name), ident(r.name), r.connLimit)
}

// alterRoleSQL repairs the attributes a non-superuser admin may change. SUPERUSER,
// REPLICATION and BYPASSRLS are omitted: mentioning them at all requires a real superuser
// (not available on RDS); they are verified instead.
func alterRoleSQL(r roleSpec) string {
	return fmt.Sprintf("ALTER ROLE %s LOGIN NOCREATEDB NOCREATEROLE CONNECTION LIMIT %d", ident(r.name), r.connLimit)
}

func roleSettingSQL(role, name, value string) string {
	return fmt.Sprintf("ALTER ROLE %s SET %s = %s", ident(role), ident(name), quoteLiteral(value))
}

// passwordSQL is built by the server with format(%I, %L), so quoting never depends on
// client-side string handling. The value is a SCRAM verifier, not the password.
const passwordSQL = "SELECT format('ALTER ROLE %I PASSWORD %L', $1::text, $2::text)"

// quoteLiteral quotes a constant for DDL that cannot take bind parameters. Only used with
// package constants (role names, settings), never with secrets.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func ensureRole(ctx context.Context, conn *pgx.Conn, r roleSpec, password string) error {
	if _, err := conn.Exec(ctx, createRoleSQL(r)); err != nil {
		return fmt.Errorf("dbbootstrap: create role %s: %w", r.name, err)
	}
	if _, err := conn.Exec(ctx, alterRoleSQL(r)); err != nil {
		return fmt.Errorf("dbbootstrap: alter role %s: %w", r.name, err)
	}
	salt, err := newSalt()
	if err != nil {
		return err
	}
	verifier, err := scramVerifier(password, salt)
	if err != nil {
		return fmt.Errorf("dbbootstrap: role %s: %w", r.name, err)
	}
	var stmt string
	if err := conn.QueryRow(ctx, passwordSQL, r.name, verifier).Scan(&stmt); err != nil {
		return fmt.Errorf("dbbootstrap: build password statement for %s: %w", r.name, err)
	}
	if _, err := conn.Exec(ctx, stmt); err != nil {
		// Do not wrap the server error text: it could quote the statement.
		return fmt.Errorf("dbbootstrap: set password for %s failed", r.name)
	}
	for _, s := range r.settings {
		if _, err := conn.Exec(ctx, roleSettingSQL(r.name, s[0], s[1])); err != nil {
			return fmt.Errorf("dbbootstrap: role %s set %s: %w", r.name, s[0], err)
		}
	}
	return nil
}

func ensureDatabase(ctx context.Context, conn *pgx.Conn, dbName string) (bool, error) {
	var owner string
	err := conn.QueryRow(ctx, "SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_catalog.pg_database WHERE datname = $1", dbName).Scan(&owner)
	created := false
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := conn.Exec(ctx, createDatabaseSQL(dbName)); err != nil {
			return false, fmt.Errorf("dbbootstrap: create database %s: %w", dbName, err)
		}
		created = true
	case err != nil:
		return false, fmt.Errorf("dbbootstrap: look up database %s: %w", dbName, err)
	case owner != OwnerRole:
		if _, err := conn.Exec(ctx, fmt.Sprintf("ALTER DATABASE %s OWNER TO %s", ident(dbName), ident(OwnerRole))); err != nil {
			return false, fmt.Errorf("dbbootstrap: change owner of %s: %w", dbName, err)
		}
	}
	for _, stmt := range databaseGrantsSQL(dbName) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return false, fmt.Errorf("dbbootstrap: %s: %w", stmt, err)
		}
	}
	return created, nil
}

func createDatabaseSQL(dbName string) string {
	return fmt.Sprintf("CREATE DATABASE %s OWNER %s TEMPLATE template0 ENCODING 'UTF8'", ident(dbName), ident(OwnerRole))
}

func databaseGrantsSQL(dbName string) []string {
	return []string{
		fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM PUBLIC", ident(dbName)),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s, %s, %s", ident(dbName), ident(OwnerRole), ident(APIRole), ident(WorkerRole)),
	}
}

func schemaSQL() []string {
	return []string{
		"ALTER SCHEMA public OWNER TO " + ident(OwnerRole),
		"REVOKE ALL ON SCHEMA public FROM PUBLIC",
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s, %s", ident(APIRole), ident(WorkerRole)),
		// citext is a trusted extension (PG13+), so the database owner may create it. Creating it
		// as maiak_owner (not as the admin, on RDS Grafana's login) keeps every object in the
		// database owned by maiak_owner. Created here so migrations need no extra rights.
		"SET ROLE " + ident(OwnerRole),
		"CREATE EXTENSION IF NOT EXISTS citext",
		"RESET ROLE",
	}
}

func ensureInDatabase(ctx context.Context, admin *pgx.ConnConfig, dbName string) error {
	cfg := admin.Copy()
	cfg.Database = dbName
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("dbbootstrap: connect to %s as admin: %w", dbName, err)
	}
	defer closeConn(conn)
	for _, stmt := range schemaSQL() {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("dbbootstrap: %s: %w", stmt, err)
		}
	}
	return nil
}

func verify(ctx context.Context, conn *pgx.Conn, dbName string, res *Result) error {
	rows, err := conn.Query(ctx, `
SELECT r.rolname, r.rolcanlogin, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolreplication,
       r.rolbypassrls, r.rolconnlimit, COALESCE(s.setconfig, '{}')
FROM pg_catalog.pg_roles r
LEFT JOIN pg_catalog.pg_db_role_setting s ON s.setrole = r.oid AND s.setdatabase = 0
WHERE r.rolname = ANY($1)
ORDER BY r.rolname`, []string{APIRole, OwnerRole, WorkerRole})
	if err != nil {
		return fmt.Errorf("dbbootstrap: verify roles: %w", err)
	}
	res.Roles, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (RoleInfo, error) {
		var ri RoleInfo
		err := row.Scan(&ri.Name, &ri.CanLogin, &ri.Superuser, &ri.CreateDB, &ri.CreateRole, &ri.Replication, &ri.BypassRLS, &ri.ConnLimit, &ri.Settings)
		return ri, err
	})
	if err != nil {
		return fmt.Errorf("dbbootstrap: verify roles: %w", err)
	}
	if len(res.Roles) != len(roles) {
		return fmt.Errorf("dbbootstrap: expected %d roles, found %d", len(roles), len(res.Roles))
	}
	var bad []string
	for _, ri := range res.Roles {
		if ri.Superuser || ri.Replication || ri.BypassRLS || ri.CreateDB || ri.CreateRole || !ri.CanLogin {
			bad = append(bad, ri.Name)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("dbbootstrap: roles with unsafe attributes (superuser/replication/bypassrls/createdb/createrole or no login): %s; a superuser must fix them", strings.Join(bad, ", "))
	}
	if err := conn.QueryRow(ctx, "SELECT pg_catalog.pg_get_userbyid(datdba) FROM pg_catalog.pg_database WHERE datname = $1", dbName).Scan(&res.DatabaseOwner); err != nil {
		return fmt.Errorf("dbbootstrap: verify database owner: %w", err)
	}
	if res.DatabaseOwner != OwnerRole {
		return fmt.Errorf("dbbootstrap: database %s is owned by %s, want %s", dbName, res.DatabaseOwner, OwnerRole)
	}
	// A runtime role inside maiak_owner would inherit table ownership, and FORCE RLS would not
	// stop a SET ROLE to the owner.
	rows, err = conn.Query(ctx, `SELECT rolname FROM pg_catalog.pg_roles
WHERE rolname = ANY($1) AND pg_catalog.pg_has_role(oid, $2, 'MEMBER') ORDER BY 1`, []string{APIRole, WorkerRole}, OwnerRole)
	if err != nil {
		return fmt.Errorf("dbbootstrap: verify memberships: %w", err)
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("dbbootstrap: verify memberships: %w", err)
	}
	if len(members) > 0 {
		return fmt.Errorf("dbbootstrap: %s must not be members of %s; revoke the membership", strings.Join(members, ", "), OwnerRole)
	}
	// pg_auth_members, not pg_has_role: a superuser "has" every role without being a member.
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_auth_members m
WHERE m.roleid = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1)
  AND m.member = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = CURRENT_USER))`, OwnerRole).Scan(&res.AdminOwnerMember); err != nil {
		return fmt.Errorf("dbbootstrap: verify admin membership: %w", err)
	}
	return nil
}

func printSummary(w io.Writer, dbName string, res *Result) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tLOGIN\tSUPERUSER\tCREATEDB\tCREATEROLE\tREPLICATION\tBYPASSRLS\tCONNLIMIT\tSETTINGS")
	for _, r := range res.Roles {
		fmt.Fprintf(tw, "%s\t%t\t%t\t%t\t%t\t%t\t%t\t%d\t%s\n", r.Name, r.CanLogin, r.Superuser, r.CreateDB, r.CreateRole,
			r.Replication, r.BypassRLS, r.ConnLimit, strings.Join(r.Settings, ","))
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "database %s owner=%s created=%t; secret created=%t\n", dbName, res.DatabaseOwner, res.DatabaseCreated, res.SecretCreated)
	fmt.Fprintf(w, "runtime roles members of %s: none; admin member of %s: %t\n", OwnerRole, OwnerRole, res.AdminOwnerMember)
}

func closeConn(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}
