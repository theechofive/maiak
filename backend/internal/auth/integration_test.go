package auth_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

var cheap = crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func TestIntegrationBootstrapAdminIdempotent(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth"))
	ctx := context.Background()
	pool := env.Pool(t, env.APIURL, 8)
	admin := env.AdminConn(t)

	hash1, _ := crypto.HashPassword("first", cheap)
	hash2, _ := crypto.HashPassword("second", cheap)

	// Several replicas starting at once.
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc, err := auth.NewService(pool, auth.Options{SessionTTL: time.Hour})
			if err == nil {
				err = svc.BootstrapAdmin(ctx, "admin", hash1)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent bootstrap: %v", err)
		}
	}

	svc, err := auth.NewService(pool, auth.Options{SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BootstrapAdmin(ctx, "admin", hash1); err != nil {
		t.Fatal(err)
	}
	// Unknown logins are verified against a dummy hash with the admin hash's cost.
	if got := svc.DummyParams(); got != cheap {
		t.Fatalf("dummy hash params = %+v, want the admin's %+v", got, cheap)
	}
	assertCounts := func(wantHash string) {
		t.Helper()
		var tenants, users, members int
		var stored, role string
		q := `SELECT (SELECT count(*) FROM tenants WHERE slug = 'default'),
		             (SELECT count(*) FROM users),
		             (SELECT count(*) FROM memberships),
		             (SELECT password_hash FROM users WHERE email = 'admin'),
		             (SELECT role FROM memberships)`
		if err := admin.QueryRow(ctx, q).Scan(&tenants, &users, &members, &stored, &role); err != nil {
			t.Fatal(err)
		}
		if tenants != 1 || users != 1 || members != 1 || role != "owner" {
			t.Fatalf("tenants=%d users=%d memberships=%d role=%s, want 1/1/1/owner", tenants, users, members, role)
		}
		if stored != wantHash {
			t.Fatal("stored password hash does not match the configured one")
		}
	}
	assertCounts(hash1)
	assertPlan := func(want string) {
		t.Helper()
		var plan string
		if err := admin.QueryRow(ctx, `SELECT plan_key FROM tenants WHERE slug = 'default'`).Scan(&plan); err != nil || plan != want {
			t.Fatalf("default tenant plan = %q (%v), want %q", plan, err, want)
		}
	}
	// The default tenant is created on the internal plan (ADR-0093).
	assertPlan("internal")

	// A changed Secret (password reset) replaces the hash; a demoted membership is restored. A
	// plan changed meanwhile is kept: bootstrap never changes the plan of an existing tenant.
	if _, err := admin.Exec(ctx, `UPDATE memberships SET role = 'viewer'`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE tenants SET plan_key = 'free'`); err != nil {
		t.Fatal(err)
	}
	if err := svc.BootstrapAdmin(ctx, "ADMIN", hash2); err != nil {
		t.Fatal(err)
	}
	assertCounts(hash2)
	assertPlan("free")
	var stored string
	if err := admin.QueryRow(ctx, `SELECT email::text FROM users`).Scan(&stored); err != nil || stored != "admin" {
		t.Fatalf("stored login = %q (err %v), want it lowercased", stored, err)
	}

	if err := svc.BootstrapAdmin(ctx, "admin", "not-a-hash"); !errors.Is(err, crypto.ErrInvalidHash) {
		t.Fatalf("invalid hash: err = %v", err)
	}
}

var testIP = netip.MustParseAddr("192.0.2.10")

func newBootstrapped(t *testing.T, env *pgtest.Env, login, hash string) *auth.Service {
	t.Helper()
	svc, err := auth.NewService(env.Pool(t, env.APIURL, 8), auth.Options{SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BootstrapAdmin(context.Background(), login, hash); err != nil {
		t.Fatal(err)
	}
	return svc
}

func login(t *testing.T, svc *auth.Service, name, password string) (auth.LoginResult, error) {
	t.Helper()
	return svc.Login(context.Background(), auth.LoginInput{Login: name, Password: password, IP: testIP})
}

func mustLogin(t *testing.T, svc *auth.Service, name, password string) string {
	t.Helper()
	res, err := login(t, svc, name, password)
	if err != nil {
		t.Fatalf("login %s: %v", name, err)
	}
	return res.Token
}

func TestIntegrationAdminLoginRotation(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_rotation"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	hash, _ := crypto.HashPassword("pw", cheap)

	svcA := newBootstrapped(t, env, "a@example.com", hash)
	tokenA := mustLogin(t, svcA, "a@example.com", "pw")

	// The Secret now names b@: a@ is disabled, its session revoked, b@ can log in.
	svcB := newBootstrapped(t, env, "b@example.com", hash)
	if _, err := svcB.Authenticate(ctx, tokenA); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("old admin session: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := login(t, svcB, "a@example.com", "pw"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old admin login: err = %v, want ErrInvalidCredentials", err)
	}
	tokenB := mustLogin(t, svcB, "b@example.com", "pw")
	if _, err := svcB.Authenticate(ctx, tokenB); err != nil {
		t.Fatalf("new admin session: %v", err)
	}

	var statusA string
	var sessionsA, rotated int
	if err := admin.QueryRow(ctx, `SELECT (SELECT status FROM users WHERE email = 'a@example.com'),
		(SELECT count(*) FROM sessions s JOIN users u ON u.id = s.user_id WHERE u.email = 'a@example.com'),
		(SELECT count(*) FROM audit_log WHERE action = 'admin.rotated'
		   AND entity_id = (SELECT id::text FROM users WHERE email = 'a@example.com'))`).Scan(&statusA, &sessionsA, &rotated); err != nil {
		t.Fatal(err)
	}
	if statusA != "disabled" || sessionsA != 0 || rotated != 1 {
		t.Fatalf("old admin: status=%s sessions=%d admin.rotated rows=%d, want disabled/0/1", statusA, sessionsA, rotated)
	}

	// Re-running with the same login changes nothing (no second audit row).
	_ = newBootstrapped(t, env, "b@example.com", hash)
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'admin.rotated'`).Scan(&rotated); err != nil || rotated != 1 {
		t.Fatalf("admin.rotated rows after rerun = %d (err %v), want 1", rotated, err)
	}

	// Rotating back re-enables a@ and retires b@.
	svcA2 := newBootstrapped(t, env, "a@example.com", hash)
	mustLogin(t, svcA2, "a@example.com", "pw")
	if _, err := svcA2.Authenticate(ctx, tokenB); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("b@ session after rotating back: err = %v", err)
	}
}

func TestIntegrationSessionRejectedForInactiveUserOrTenant(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_inactive"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	hash, _ := crypto.HashPassword("pw", cheap)
	svc := newBootstrapped(t, env, "admin", hash)

	cases := []struct {
		name, disable, enable string
	}{
		{"disabled user", `UPDATE users SET status = 'disabled'`, `UPDATE users SET status = 'active'`},
		{"suspended tenant", `UPDATE tenants SET status = 'suspended'`, `UPDATE tenants SET status = 'active'`},
		{"deleted tenant", `UPDATE tenants SET deleted_at = now()`, `UPDATE tenants SET deleted_at = NULL`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := mustLogin(t, svc, "admin", "pw")
			if _, err := svc.Authenticate(ctx, token); err != nil {
				t.Fatalf("before: %v", err)
			}
			pgtest.MustExec(t, admin, c.disable)
			if _, err := svc.Authenticate(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Fatalf("after %q: err = %v, want ErrUnauthenticated", c.disable, err)
			}
			pgtest.MustExec(t, admin, c.enable)
			if _, err := svc.Authenticate(ctx, token); err != nil {
				t.Fatalf("after re-enabling: %v", err)
			}
		})
	}
}

func TestIntegrationPasswordChangeRevokesSessions(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_pwchange"))
	ctx := context.Background()
	hash1, _ := crypto.HashPassword("first", cheap)
	hash2, _ := crypto.HashPassword("second", cheap)

	svc := newBootstrapped(t, env, "admin", hash1)
	token := mustLogin(t, svc, "admin", "first")

	// Same hash again (a restart): the session survives.
	svc = newBootstrapped(t, env, "admin", hash1)
	if _, err := svc.Authenticate(ctx, token); err != nil {
		t.Fatalf("restart with the same hash revoked the session: %v", err)
	}

	svc = newBootstrapped(t, env, "admin", hash2)
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("session after password change: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := login(t, svc, "admin", "first"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("old password: err = %v", err)
	}
	mustLogin(t, svc, "admin", "second")
}

// Concurrent wrong passwords for one login cannot run more verifications than the limit:
// every attempt is either rejected by the limiter or verified (and then fails).
func TestIntegrationLoginLimitHoldsUnderConcurrency(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_ratelimit"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	hash, _ := crypto.HashPassword("pw", cheap)
	const limit = 10
	svc, err := auth.NewService(env.Pool(t, env.APIURL, 8), auth.Options{
		SessionTTL:    time.Hour,
		IPLimiter:     auth.NewLimiter(1000, 15*time.Minute, 1000, clock.System{}),
		LoginLimiter:  auth.NewLimiter(limit, 15*time.Minute, 1000, clock.System{}),
		HashQueueWait: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BootstrapAdmin(ctx, "admin", hash); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var verified, limited, other atomic.Int32
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Login(ctx, auth.LoginInput{Login: "Admin", Password: "wrong", IP: testIP})
			switch {
			case errors.Is(err, auth.ErrInvalidCredentials):
				verified.Add(1)
			case errors.Is(err, auth.ErrRateLimited):
				limited.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	wg.Wait()
	if verified.Load() > limit || other.Load() != 0 || verified.Load()+limited.Load() != 200 {
		t.Fatalf("verified=%d rate_limited=%d other=%d; want at most %d verifications", verified.Load(), limited.Load(), other.Load(), limit)
	}
	var failures int32
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'auth.login.failed'`).Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != verified.Load() {
		t.Fatalf("audit rows = %d, verifications = %d", failures, verified.Load())
	}
	// The correct password is now rate limited too, for this login.
	if _, err := login(t, svc, "admin", "pw"); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("login after the limit: err = %v", err)
	}
}

// A registered user logs in to their own tenant; a suspended or deleted tenant refuses the login
// with the generic error, and a user without any membership cannot log in.
func TestIntegrationRegisteredLoginBindsToOwnTenant(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_registered"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	hash, _ := crypto.HashPassword("pw", cheap)
	svc := newBootstrapped(t, env, "admin", hash)
	const password = "registered user password"

	reg, err := svc.Register(ctx, auth.RegisterInput{Email: "User@Example.com", Password: password, IP: testIP})
	if err != nil {
		t.Fatal(err)
	}
	if reg.Tenant.Slug == "default" || reg.Role != auth.RoleOwner || reg.User.Login != "user@example.com" {
		t.Fatalf("register result: %+v", reg.SessionInfo)
	}
	res, err := login(t, svc, "USER@example.com", password)
	if err != nil || res.Tenant.ID != reg.Tenant.ID {
		t.Fatalf("login: tenant %v, err %v; want %v", res.Tenant.ID, err, reg.Tenant.ID)
	}
	if p, err := svc.Authenticate(ctx, res.Token); err != nil || p.TenantID != reg.Tenant.ID {
		t.Fatalf("authenticate: %+v %v", p, err)
	}
	if _, err := svc.Register(ctx, auth.RegisterInput{Email: "user@example.COM", Password: password, IP: testIP}); !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("duplicate: err = %v", err)
	}

	// A suspended or deleted tenant (P-04) is reported only to the right password; a wrong one,
	// or a user without membership, gets the generic failure.
	for _, c := range []struct {
		disable, enable string
		want            error
	}{
		{`UPDATE tenants SET status = 'suspended' WHERE id = $1`, `UPDATE tenants SET status = 'active' WHERE id = $1`, auth.ErrAccountSuspended},
		{`UPDATE tenants SET deleted_at = now() WHERE id = $1`, `UPDATE tenants SET deleted_at = NULL WHERE id = $1`, auth.ErrAccountSuspended},
		{`DELETE FROM memberships WHERE tenant_id = $1`, `INSERT INTO memberships (tenant_id, user_id, role) SELECT $1, id, 'owner' FROM users WHERE email = 'user@example.com'`, auth.ErrInvalidCredentials},
	} {
		if _, err := admin.Exec(ctx, c.disable, reg.Tenant.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := login(t, svc, "user@example.com", password); !errors.Is(err, c.want) {
			t.Fatalf("after %q: err = %v, want %v", c.disable, err, c.want)
		}
		if _, err := login(t, svc, "user@example.com", password+"x"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("wrong password after %q: err = %v, want ErrInvalidCredentials", c.disable, err)
		}
		if _, err := admin.Exec(ctx, c.enable, reg.Tenant.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := login(t, svc, "user@example.com", password); err != nil {
			t.Fatalf("after %q: %v", c.enable, err)
		}
	}
	// The admin is unaffected.
	if res := mustLogin(t, svc, "admin", "pw"); res == "" {
		t.Fatal("admin login")
	}
}

// A stranger who registered the email that the operator later configures as MAIAK_ADMIN_LOGIN is
// not adopted: BootstrapAdmin fails and writes nothing (P-07, ADR-0095).
func TestIntegrationBootstrapRefusesRegisteredStranger(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_adopt_refused"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	hash, _ := crypto.HashPassword("pw", cheap)
	svc := newBootstrapped(t, env, "admin", hash)
	adminToken := mustLogin(t, svc, "admin", "pw")
	const strangerPassword = "stranger's own password"
	reg, err := svc.Register(ctx, auth.RegisterInput{Email: "ops@company.com", Password: strangerPassword, IP: testIP})
	if err != nil {
		t.Fatal(err)
	}

	snapshot := func() string {
		t.Helper()
		var s string
		if err := admin.QueryRow(ctx, `SELECT concat_ws('|',
			(SELECT string_agg(email || ':' || status || ':' || coalesce(password_hash, ''), ',' ORDER BY email) FROM users),
			(SELECT string_agg(tenant_id || ':' || user_id || ':' || role, ',' ORDER BY tenant_id, user_id) FROM memberships),
			(SELECT string_agg(id::text, ',' ORDER BY id) FROM sessions),
			(SELECT count(*) FROM audit_log))`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	other, err := auth.NewService(env.Pool(t, env.APIURL, 2), auth.Options{SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range []string{"ops@company.com", " OPS@Company.com "} {
		if err := other.BootstrapAdmin(ctx, configured, hash); !errors.Is(err, auth.ErrAdminLoginInUse) {
			t.Fatalf("bootstrap with %q: err = %v, want ErrAdminLoginInUse", configured, err)
		}
	}
	if after := snapshot(); after != before {
		t.Fatalf("refused bootstrap changed data:\nbefore %s\nafter  %s", before, after)
	}
	// Both sides keep working as before: the admin in default, the stranger in its own tenant.
	if p, err := svc.Authenticate(ctx, adminToken); err != nil || p.TenantID == reg.Tenant.ID {
		t.Fatalf("admin session: %+v %v", p, err)
	}
	res, err := login(t, svc, "ops@company.com", strangerPassword)
	if err != nil || res.Tenant.ID != reg.Tenant.ID {
		t.Fatalf("stranger login: tenant %v err %v", res.Tenant.ID, err)
	}
}

// A user who is only a member of the default tenant may be adopted as the admin: it becomes the
// owner, every one of its sessions is deleted, the previous admin is retired, and its login lands
// in the default tenant even with an older membership elsewhere (which, once it exists, makes
// the next bootstrap refuse to start).
func TestIntegrationBootstrapAdoptsDefaultOnlyUser(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_adopt_allowed"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	oldHash, _ := crypto.HashPassword("old", cheap)
	opsHash, _ := crypto.HashPassword("ops first", cheap)
	newHash, _ := crypto.HashPassword("ops second", cheap)
	svc := newBootstrapped(t, env, "admin", oldHash)
	oldAdminToken := mustLogin(t, svc, "admin", "old")
	pgtest.MustExec(t, admin,
		`INSERT INTO users (id, email, password_hash) VALUES ('01920000-0000-7000-8000-0000000000e1', 'ops@company.com', '`+opsHash+`')`,
		`INSERT INTO memberships (tenant_id, user_id, role) SELECT id, '01920000-0000-7000-8000-0000000000e1', 'viewer' FROM tenants WHERE slug = 'default'`)
	opsToken := mustLogin(t, svc, "ops@company.com", "ops first")

	// Same hash as stored: the adoption alone must revoke the session.
	svc = newBootstrapped(t, env, "ops@company.com", opsHash)
	for name, token := range map[string]string{"adopted user": opsToken, "previous admin": oldAdminToken} {
		if _, err := svc.Authenticate(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Fatalf("%s session: err = %v, want ErrUnauthenticated", name, err)
		}
	}
	var role, oldStatus string
	if err := admin.QueryRow(ctx, `SELECT (SELECT m.role FROM memberships m JOIN users u ON u.id = m.user_id WHERE u.email = 'ops@company.com'),
		(SELECT status FROM users WHERE email = 'admin')`).Scan(&role, &oldStatus); err != nil {
		t.Fatal(err)
	}
	if role != auth.RoleOwner || oldStatus != "disabled" {
		t.Fatalf("role=%s previous admin=%s, want owner/disabled", role, oldStatus)
	}
	res, err := login(t, svc, "ops@company.com", "ops first")
	if err != nil || res.Tenant.Slug != "default" {
		t.Fatalf("adopted admin login: %+v %v", res.Tenant, err)
	}

	// An older membership elsewhere does not steer the admin's login away from default.
	pgtest.MustExec(t, admin,
		`INSERT INTO tenants (id, slug, name) VALUES ('01920000-0000-7000-8000-0000000000ef', 'elsewhere', 'Elsewhere')`,
		`INSERT INTO memberships (tenant_id, user_id, role, created_at)
		 VALUES ('01920000-0000-7000-8000-0000000000ef', '01920000-0000-7000-8000-0000000000e1', 'owner', now() - interval '1 year')`)
	if res, err := login(t, svc, "ops@company.com", "ops first"); err != nil || res.Tenant.Slug != "default" {
		t.Fatalf("login with an older membership: %+v %v", res.Tenant, err)
	}
	other, err := auth.NewService(env.Pool(t, env.APIURL, 2), auth.Options{SessionTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.BootstrapAdmin(ctx, "ops@company.com", newHash); !errors.Is(err, auth.ErrAdminLoginInUse) {
		t.Fatalf("bootstrap with a membership elsewhere: err = %v, want ErrAdminLoginInUse", err)
	}
}

// The platform-admin flag (P-04, ADR-0097) follows MAIAK_ADMIN_LOGIN: BootstrapAdmin sets it for
// the configured admin at every start and clears it for a retired one; a registered user never
// has it. Login and Me report it.
func TestIntegrationPlatformAdminFlag(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_platform_admin"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	hash, _ := crypto.HashPassword("pw", cheap)
	flags := func() string {
		t.Helper()
		var s string
		if err := admin.QueryRow(ctx, `SELECT string_agg(email::text || '=' || is_platform_admin::text, ',' ORDER BY email) FROM users`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	svc := newBootstrapped(t, env, "admin", hash)
	res, err := login(t, svc, "admin", "pw")
	if err != nil || !res.User.IsPlatformAdmin {
		t.Fatalf("admin login: %+v %v", res.User, err)
	}
	p, err := svc.Authenticate(ctx, res.Token)
	if err != nil {
		t.Fatal(err)
	}
	if me, err := svc.Me(ctx, p); err != nil || !me.User.IsPlatformAdmin {
		t.Fatalf("me: %+v %v", me.User, err)
	}
	if ok, err := svc.IsPlatformAdmin(ctx, p.UserID); err != nil || !ok {
		t.Fatalf("IsPlatformAdmin(admin) = %v, %v", ok, err)
	}

	reg, err := svc.Register(ctx, auth.RegisterInput{Email: "user@example.com", Password: "registered user password", IP: testIP})
	if err != nil || reg.User.IsPlatformAdmin {
		t.Fatalf("register: %+v %v", reg.User, err)
	}
	if ok, err := svc.IsPlatformAdmin(ctx, reg.User.ID); err != nil || ok {
		t.Fatalf("IsPlatformAdmin(registered) = %v, %v", ok, err)
	}
	if got := flags(); got != "admin=true,user@example.com=false" {
		t.Fatalf("flags = %s", got)
	}

	// A manual reset of the flag is repaired at the next start (idempotent otherwise).
	pgtest.MustExec(t, admin, `UPDATE users SET is_platform_admin = false`)
	_ = newBootstrapped(t, env, "admin", hash)
	if got := flags(); got != "admin=true,user@example.com=false" {
		t.Fatalf("flags after restart = %s", got)
	}

	// Rotating the login moves the flag; the retired admin loses it (and is disabled).
	_ = newBootstrapped(t, env, "root", hash)
	if got := flags(); got != "admin=false,root=true,user@example.com=false" {
		t.Fatalf("flags after rotation = %s", got)
	}
	if ok, err := svc.IsPlatformAdmin(ctx, p.UserID); err != nil || ok {
		t.Fatalf("IsPlatformAdmin(retired admin) = %v, %v", ok, err)
	}
	if ok, err := svc.IsPlatformAdmin(ctx, uuid.New()); err != nil || ok {
		t.Fatalf("IsPlatformAdmin(unknown) = %v, %v", ok, err)
	}
}

// HashPassword uses the admin hash's parameters and the bounded argon2 queue.
func TestIntegrationHashPassword(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_hashpw"))
	hash, _ := crypto.HashPassword("pw", cheap)
	svc := newBootstrapped(t, env, "admin", hash)
	phc, err := svc.HashPassword(context.Background(), "temporary password 123")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := crypto.ParseArgon2idHash(phc)
	if err != nil || parsed.Params != cheap || !parsed.Verify("temporary password 123") {
		t.Fatalf("hash %q: params %+v err %v", phc, parsed, err)
	}
}

// A login whose password was verified before a password change never keeps a session (P-09,
// ADR-0098): the login is paused between the verification and the session transaction while a
// rotation commits, or while a reset is still in flight in a second transaction.
func TestIntegrationLoginRaceWithPasswordReset(t *testing.T) {
	env := pgtest.New(t, pgtest.DBName("auth_login_race"))
	ctx := context.Background()
	admin := env.AdminConn(t)
	hash1, _ := crypto.HashPassword("first", cheap)
	hash2, _ := crypto.HashPassword("second", cheap)
	hash3, _ := crypto.HashPassword("third", cheap)
	svc := newBootstrapped(t, env, "admin", hash1)

	sessions := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM sessions AS s JOIN users AS u ON u.id = s.user_id WHERE u.email = 'admin'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// pausedLogin starts a login that stops after its password verification; resume lets it go on.
	pausedLogin := func(password string) (resume func(), done <-chan error) {
		t.Helper()
		paused, cont := make(chan struct{}), make(chan struct{})
		svc.SetAfterVerify(func() { close(paused); <-cont })
		out := make(chan error, 1)
		go func() {
			_, err := svc.Login(ctx, auth.LoginInput{Login: "admin", Password: password, IP: testIP})
			out <- err
		}()
		<-paused
		return func() { close(cont) }, out
	}
	t.Cleanup(func() { svc.SetAfterVerify(func() {}) })

	t.Run("a rotation committed after the verification", func(t *testing.T) {
		resume, done := pausedLogin("first")
		// Another replica rotates the password from the Secret.
		newBootstrapped(t, env, "admin", hash2)
		resume()
		if err := <-done; !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("login verified before the rotation: err = %v, want ErrInvalidCredentials", err)
		}
		if n := sessions(); n != 0 {
			t.Fatalf("sessions = %d, want 0", n)
		}
	})

	t.Run("a reset in flight in a second transaction", func(t *testing.T) {
		resume, done := pausedLogin("second")
		tx, err := admin.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		// What admin_reset_user_password does: new hash and updated_at, then the sessions go.
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $1, updated_at = now() WHERE email = 'admin'`, hash3); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT auth_revoke_user_sessions(id) FROM users WHERE email = 'admin'`); err != nil {
			t.Fatal(err)
		}
		resume()
		// The session transaction waits for the row lock of the reset.
		waitForLockWait(t, env, "FOR SHARE")
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("login verified before the reset: err = %v, want ErrInvalidCredentials", err)
		}
		if n := sessions(); n != 0 {
			t.Fatalf("sessions = %d, want 0", n)
		}
	})

	t.Run("an unchanged credential still logs in", func(t *testing.T) {
		resume, done := pausedLogin("third")
		resume()
		if err := <-done; err != nil {
			t.Fatalf("login: %v", err)
		}
		if n := sessions(); n != 1 {
			t.Fatalf("sessions = %d, want 1", n)
		}
	})
}

// waitForLockWait waits until a backend of the test database waits for a lock in a query that
// contains fragment.
func waitForLockWait(t *testing.T, env *pgtest.Env, fragment string) {
	t.Helper()
	conn := env.AdminConn(t)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := conn.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND strpos(query, $1) > 0)`, fragment).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no backend waits for a lock in a query with %q", fragment)
}
