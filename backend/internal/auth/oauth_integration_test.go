package auth_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/auth"
	"github.com/theechofive/maiak/backend/internal/authz"
	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

type oauthEnv struct {
	t     *testing.T
	ctx   context.Context
	svc   *auth.Service
	admin *pgx.Conn
}

func newOAuthEnv(t *testing.T, name string, opts auth.Options) *oauthEnv {
	t.Helper()
	env := pgtest.New(t, pgtest.DBName(name))
	ctx := context.Background()
	opts.SessionTTL = time.Hour
	svc, err := auth.NewService(env.Pool(t, env.APIURL, 8), opts)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := crypto.HashPassword("admin password", cheap)
	if err := svc.BootstrapAdmin(ctx, "admin", hash); err != nil {
		t.Fatal(err)
	}
	return &oauthEnv{t: t, ctx: ctx, svc: svc, admin: env.AdminConn(t)}
}

var oauthIP = netip.MustParseAddr("198.51.100.20")

func (e *oauthEnv) login(in auth.OAuthLoginInput) (auth.LoginResult, error) {
	in.IP = oauthIP
	return e.svc.OAuthLogin(e.ctx, in)
}

func (e *oauthEnv) principal(res auth.LoginResult) auth.Principal {
	e.t.Helper()
	p, err := e.svc.Authenticate(e.ctx, res.Token)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *oauthEnv) adminLogin() auth.LoginResult {
	e.t.Helper()
	res, err := e.svc.Login(e.ctx, auth.LoginInput{Login: "admin", Password: "admin password", IP: oauthIP})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *oauthEnv) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.admin.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func google(sub, email, hd string) auth.OAuthIdentity {
	return auth.OAuthIdentity{Provider: "google", Subject: sub, Email: email, HostedDomain: hd, DisplayName: "G " + sub}
}

func github(sub, email string) auth.OAuthIdentity {
	return auth.OAuthIdentity{Provider: "github", Subject: sub, Email: email}
}

func TestIntegrationOAuthLoginRules(t *testing.T) {
	e := newOAuthEnv(t, "auth_oauth_login", auth.Options{})

	// Nothing allows a new account: not_invited, audited, nothing written.
	if _, err := e.login(auth.OAuthLoginInput{Identity: google("g-1", "new@example.com", "")}); !errors.Is(err, auth.ErrOAuthNotInvited) {
		t.Fatalf("no invite, registration off: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM users WHERE email = 'new@example.com'`); n != 0 {
		t.Fatal("user created without permission")
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE action = 'auth.oauth_failed' AND metadata->>'reason' = 'not_invited'`); n != 1 {
		t.Fatalf("not_invited audit rows = %d", n)
	}

	// A password account with the same email is never linked automatically (pre-hijacking).
	pw, err := e.svc.Register(e.ctx, auth.RegisterInput{Email: "victim@example.com", Password: "a long enough password", IP: oauthIP})
	if err != nil {
		t.Fatal(err)
	}
	for _, reg := range []bool{false, true} {
		_, err := e.login(auth.OAuthLoginInput{Identity: google("g-victim", "Victim@Example.com", ""), RegistrationEnabled: reg})
		if !errors.Is(err, auth.ErrOAuthAccountExists) {
			t.Fatalf("registration=%v: existing email answered %v, want account_exists", reg, err)
		}
	}
	if n := e.count(`SELECT count(*) FROM auth_identities WHERE user_id = $1`, pw.User.ID); n != 0 {
		t.Fatal("identity linked automatically to the password account")
	}

	// Registration open: a new organization with the user as owner, verified email, no password.
	res, err := e.login(auth.OAuthLoginInput{Identity: google("g-2", "Owner@Example.org", ""), RegistrationEnabled: true, UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Role != auth.RoleOwner || res.User.Login != "owner@example.org" || res.Token == "" || res.Tenant.Slug == "default" {
		t.Fatalf("registered: %+v", res.SessionInfo)
	}
	var verified *time.Time
	var hash *string
	if err := e.admin.QueryRow(e.ctx, `SELECT email_verified_at, password_hash FROM users WHERE id = $1`, res.User.ID).Scan(&verified, &hash); err != nil {
		t.Fatal(err)
	}
	if verified == nil || hash != nil {
		t.Fatalf("oauth user: verified=%v password=%v", verified, hash)
	}
	if n := e.count(`SELECT count(*) FROM projects WHERE tenant_id = $1 AND is_default`, res.Tenant.ID); n != 1 {
		t.Fatal("registered organization has no Default project")
	}

	// The next sign-in finds the user by subject, even when the provider reports another email.
	again, err := e.login(auth.OAuthLoginInput{Identity: google("g-2", "renamed@example.org", "")})
	if err != nil {
		t.Fatal(err)
	}
	if again.User.ID != res.User.ID || again.Tenant.ID != res.Tenant.ID {
		t.Fatalf("second sign-in: %+v", again.SessionInfo)
	}
	var email string
	var last *time.Time
	if err := e.admin.QueryRow(e.ctx, `SELECT email, last_login_at FROM auth_identities WHERE provider = 'google' AND subject = 'g-2'`).Scan(&email, &last); err != nil {
		t.Fatal(err)
	}
	if email != "renamed@example.org" || last == nil {
		t.Fatalf("identity not refreshed: %s %v", email, last)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE action = 'auth.oauth_login' AND actor_user_id = $1`, res.User.ID); n != 2 {
		t.Fatalf("oauth_login audit rows = %d", n)
	}

	// The same subject at another provider is another identity.
	if _, err := e.login(auth.OAuthLoginInput{Identity: github("g-2", "owner@example.org")}); !errors.Is(err, auth.ErrOAuthAccountExists) {
		t.Fatalf("github with an existing email: %v", err)
	}

	// Emails that are not valid logins are refused.
	if _, err := e.login(auth.OAuthLoginInput{Identity: github("77", "weird name@example.org"), RegistrationEnabled: true}); !errors.Is(err, auth.ErrOAuthEmailUnusable) {
		t.Fatalf("unusable email: %v", err)
	}

	// A disabled user cannot sign in with its identity.
	pgtest.MustExec(t, e.admin, `UPDATE users SET status = 'disabled' WHERE id = '`+res.User.ID.String()+`'`)
	if _, err := e.login(auth.OAuthLoginInput{Identity: google("g-2", "renamed@example.org", "")}); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("disabled user: %v", err)
	}
}

func TestIntegrationOAuthInvitation(t *testing.T) {
	e := newOAuthEnv(t, "auth_oauth_invite", auth.Options{})
	admin := e.adminLogin()
	actor := auth.Actor{TenantID: admin.Tenant.ID, UserID: admin.User.ID}
	_, token, err := e.svc.CreateInvitation(e.ctx, actor, auth.RoleMember, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CheckInvitation(e.ctx, "not-a-token", oauthIP); !errors.Is(err, auth.ErrInvitationInvalid) {
		t.Fatalf("bad token: %v", err)
	}
	h, err := e.svc.CheckInvitation(e.ctx, token, oauthIP)
	if err != nil {
		t.Fatal(err)
	}

	// An existing email is still account_exists with an invitation, and the invitation stays open.
	if _, err := e.svc.Register(e.ctx, auth.RegisterInput{Email: "taken@example.com", Password: "a long enough password", IP: oauthIP}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.login(auth.OAuthLoginInput{Identity: github("1", "taken@example.com"), InviteHash: h}); !errors.Is(err, auth.ErrOAuthAccountExists) {
		t.Fatalf("invited sign-in with a taken email: %v", err)
	}
	res, err := e.login(auth.OAuthLoginInput{Identity: github("1001", "invited@example.com"), InviteHash: h})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tenant.ID != admin.Tenant.ID || res.Role != auth.RoleMember {
		t.Fatalf("invited sign-in: %+v", res.SessionInfo)
	}
	if n := e.count(`SELECT count(*) FROM invitations WHERE accepted_by = $1`, res.User.ID); n != 1 {
		t.Fatal("invitation not used up")
	}
	// The invitation is used: another identity cannot reuse it.
	if _, err := e.login(auth.OAuthLoginInput{Identity: github("1002", "second@example.com"), InviteHash: h}); !errors.Is(err, auth.ErrInvitationInvalid) {
		t.Fatalf("reused invitation: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM users WHERE email = 'second@example.com'`); n != 0 {
		t.Fatal("user created with a used invitation")
	}

	// A linked user joins another organization through an invitation.
	other, err := e.login(auth.OAuthLoginInput{Identity: google("g-org", "boss@example.net", ""), RegistrationEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, token2, err := e.svc.CreateInvitation(e.ctx, auth.Actor{TenantID: other.Tenant.ID, UserID: other.User.ID}, auth.RoleViewer, "")
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := e.svc.CheckInvitation(e.ctx, token2, oauthIP)
	joined, err := e.login(auth.OAuthLoginInput{Identity: github("1001", "invited@example.com"), InviteHash: h2})
	if err != nil {
		t.Fatal(err)
	}
	if joined.User.ID != res.User.ID || joined.Tenant.ID != other.Tenant.ID || joined.Role != auth.RoleViewer {
		t.Fatalf("linked user joining: %+v", joined.SessionInfo)
	}
}

func TestIntegrationOAuthDomains(t *testing.T) {
	e := newOAuthEnv(t, "auth_oauth_domains", auth.Options{})
	admin := e.adminLogin()
	ap := e.principal(admin)
	owner := auth.Actor{TenantID: admin.Tenant.ID, UserID: admin.User.ID}

	// Claiming needs a Google Workspace sign-in with that hd (proof of control).
	if _, err := e.svc.CreateAuthDomain(e.ctx, owner, "Example.com", "", true); !errors.Is(err, auth.ErrDomainNotVerified) {
		t.Fatalf("claim without proof: %v", err)
	}
	if err := e.svc.LinkIdentity(e.ctx, ap, google("g-admin", "it@example.com", "example.com"), oauthIP); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateAuthDomain(e.ctx, owner, "other.com", "", true); !errors.Is(err, auth.ErrDomainNotVerified) {
		t.Fatalf("claim of another domain: %v", err)
	}
	if _, err := e.svc.CreateAuthDomain(e.ctx, owner, "not a domain", "", true); err == nil {
		t.Fatal("invalid domain accepted")
	}
	d, err := e.svc.CreateAuthDomain(e.ctx, owner, "Example.com.", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Domain != "example.com" || d.DefaultRole != auth.RoleViewer || !d.Enabled {
		t.Fatalf("domain: %+v", d)
	}

	// Another organization's owner with proof still cannot take it (global unique).
	rival, err := e.login(auth.OAuthLoginInput{Identity: google("g-rival", "rival@example.com", "example.com"), RegistrationEnabled: false})
	if err != nil {
		// Joined the admin's organization through the domain, so it is not an owner anywhere.
		t.Fatal(err)
	}
	if rival.Tenant.ID != admin.Tenant.ID || rival.Role != auth.RoleViewer {
		t.Fatalf("domain join: %+v", rival.SessionInfo)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE action = 'member.joined' AND actor_user_id = $1 AND metadata->>'via' = 'auth_domain'`, rival.User.ID); n != 1 {
		t.Fatal("domain join not audited")
	}
	second, err := e.login(auth.OAuthLoginInput{Identity: google("g-second", "boss@second.org", "second.org"), RegistrationEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	secondOwner := auth.Actor{TenantID: second.Tenant.ID, UserID: second.User.ID}
	pgtest.MustExec(t, e.admin, `UPDATE auth_identities SET hosted_domain = 'example.com' WHERE subject = 'g-second'`)
	if _, err := e.svc.CreateAuthDomain(e.ctx, secondOwner, "example.com", "", true); !errors.Is(err, auth.ErrDomainTaken) {
		t.Fatalf("second claim: %v", err)
	}

	// Cross-tenant isolation: the second organization neither sees nor changes the first's domain.
	list, err := e.svc.ListAuthDomains(e.ctx, second.Tenant.ID)
	if err != nil || len(list) != 0 {
		t.Fatalf("second organization lists %v %v", list, err)
	}
	member := "member"
	if _, err := e.svc.UpdateAuthDomain(e.ctx, secondOwner, d.ID, &member, nil); !errors.Is(err, auth.ErrAuthDomainNotFound) {
		t.Fatalf("cross-tenant update: %v", err)
	}
	if err := e.svc.DeleteAuthDomain(e.ctx, secondOwner, d.ID); !errors.Is(err, auth.ErrAuthDomainNotFound) {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	// Only owners: a member of the first organization is refused by the service as well.
	if _, err := e.svc.UpdateAuthDomain(e.ctx, auth.Actor{TenantID: admin.Tenant.ID, UserID: rival.User.ID}, d.ID, &member, nil); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("viewer update: %v", err)
	}

	// hd rules: another hd, no hd (Gmail), a GitHub account and email_verified handled upstream.
	for name, id := range map[string]auth.OAuthIdentity{
		"hd mismatch": google("g-x", "x@other.com", "other.com"),
		"no hd":       google("g-y", "y@gmail.com", ""),
		"github":      github("555", "z@example.com"),
	} {
		if _, err := e.login(auth.OAuthLoginInput{Identity: id}); !errors.Is(err, auth.ErrOAuthNotInvited) {
			t.Errorf("%s: %v, want not_invited", name, err)
		}
	}

	// Role change applies to the next joiner; a disabled domain joins nobody.
	if _, err := e.svc.UpdateAuthDomain(e.ctx, owner, d.ID, &member, nil); err != nil {
		t.Fatal(err)
	}
	m, err := e.login(auth.OAuthLoginInput{Identity: google("g-m", "m@example.com", "example.com")})
	if err != nil || m.Role != auth.RoleMember {
		t.Fatalf("member join: %v %+v", err, m.SessionInfo)
	}
	off := false
	if _, err := e.svc.UpdateAuthDomain(e.ctx, owner, d.ID, nil, &off); err != nil {
		t.Fatal(err)
	}
	if _, err := e.login(auth.OAuthLoginInput{Identity: google("g-n", "n@example.com", "example.com")}); !errors.Is(err, auth.ErrOAuthNotInvited) {
		t.Fatalf("disabled domain: %v", err)
	}
	if err := e.svc.DeleteAuthDomain(e.ctx, owner, d.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE action = 'org.auth_domain_changed' AND tenant_id = $1`, admin.Tenant.ID); n != 4 {
		t.Fatalf("domain audit rows = %d, want 4", n)
	}
}

func TestIntegrationOAuthLinkUnlink(t *testing.T) {
	e := newOAuthEnv(t, "auth_oauth_link", auth.Options{})
	admin := e.adminLogin()
	ap := e.principal(admin)

	// An OAuth-only user cannot remove its only identity.
	u, err := e.login(auth.OAuthLoginInput{Identity: google("g-1", "solo@example.com", ""), RegistrationEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	up := e.principal(u)
	if err := e.svc.UnlinkIdentity(e.ctx, up, "google", oauthIP); !errors.Is(err, auth.ErrLastLoginMethod) {
		t.Fatalf("unlink the only identity: %v", err)
	}
	if err := e.svc.UnlinkIdentity(e.ctx, up, "github", oauthIP); !errors.Is(err, auth.ErrIdentityNotFound) {
		t.Fatalf("unlink a missing identity: %v", err)
	}

	// Linking: someone else's identity, a second identity of one provider, the same one again.
	if err := e.svc.LinkIdentity(e.ctx, ap, google("g-1", "solo@example.com", ""), oauthIP); !errors.Is(err, auth.ErrIdentityInUse) {
		t.Fatalf("link another user's identity: %v", err)
	}
	if err := e.svc.LinkIdentity(e.ctx, up, github("9", "solo@example.com"), oauthIP); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.LinkIdentity(e.ctx, up, github("9", "solo@example.com"), oauthIP); err != nil {
		t.Fatalf("relinking the same identity: %v", err)
	}
	if err := e.svc.LinkIdentity(e.ctx, up, github("10", "solo@example.com"), oauthIP); !errors.Is(err, auth.ErrAlreadyLinked) {
		t.Fatalf("second github identity: %v", err)
	}
	m, err := e.svc.ListIdentities(e.ctx, up)
	if err != nil || len(m.Identities) != 2 || m.PasswordSet || !m.PasswordLoginEnabled {
		t.Fatalf("identities: %+v %v", m, err)
	}
	// With two identities one may go, then the other is the last.
	if err := e.svc.UnlinkIdentity(e.ctx, up, "google", oauthIP); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UnlinkIdentity(e.ctx, up, "github", oauthIP); !errors.Is(err, auth.ErrLastLoginMethod) {
		t.Fatalf("unlink the last identity: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM audit_log WHERE action IN ('auth.identity_linked', 'auth.identity_unlinked') AND actor_user_id = $1`, u.User.ID); n != 2 {
		t.Fatalf("link audit rows = %d", n)
	}

	// A password user may unlink its only identity; linking with its own email marks it verified.
	pw, err := e.svc.Register(e.ctx, auth.RegisterInput{Email: "pw@example.com", Password: "a long enough password", IP: oauthIP})
	if err != nil {
		t.Fatal(err)
	}
	pp := e.principal(pw)
	if err := e.svc.LinkIdentity(e.ctx, pp, google("g-pw", "PW@example.com", ""), oauthIP); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM users WHERE id = $1 AND email_verified_at IS NOT NULL`, pw.User.ID); n != 1 {
		t.Fatal("linking with the same verified email did not mark it verified")
	}
	// Now the password user signs in with Google directly.
	if res, err := e.login(auth.OAuthLoginInput{Identity: google("g-pw", "pw@example.com", "")}); err != nil || res.User.ID != pw.User.ID {
		t.Fatalf("sign-in after link: %v", err)
	}
	if err := e.svc.UnlinkIdentity(e.ctx, pp, "google", oauthIP); err != nil {
		t.Fatalf("password user unlink: %v", err)
	}
}

func TestIntegrationPasswordLoginDisabled(t *testing.T) {
	e := newOAuthEnv(t, "auth_oauth_pwoff", auth.Options{PasswordLoginDisabled: true})
	// The break-glass admin keeps the password.
	admin := e.adminLogin()
	if _, err := e.svc.Register(e.ctx, auth.RegisterInput{Email: "x@example.com", Password: "a long enough password", IP: oauthIP}); !errors.Is(err, auth.ErrPasswordLoginDisabled) {
		t.Fatalf("register: %v", err)
	}
	u, err := e.login(auth.OAuthLoginInput{Identity: google("g-1", "u@example.com", ""), RegistrationEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	pgtest.MustExec(t, e.admin, `UPDATE users SET password_hash = (SELECT password_hash FROM users WHERE email = 'admin') WHERE email = 'u@example.com'`)
	if _, err := e.svc.Login(e.ctx, auth.LoginInput{Login: "u@example.com", Password: "admin password", IP: oauthIP}); !errors.Is(err, auth.ErrPasswordLoginDisabled) {
		t.Fatalf("non-admin password login: %v", err)
	}
	// A password that cannot be used does not count for the unlink guard; the admin's does.
	up := e.principal(u)
	if err := e.svc.UnlinkIdentity(e.ctx, up, "google", oauthIP); !errors.Is(err, auth.ErrLastLoginMethod) {
		t.Fatalf("unlink with an unusable password: %v", err)
	}
	ap := e.principal(admin)
	if err := e.svc.LinkIdentity(e.ctx, ap, github("1", "admin@example.com"), oauthIP); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.UnlinkIdentity(e.ctx, ap, "github", oauthIP); err != nil {
		t.Fatalf("admin unlink: %v", err)
	}
}

func TestIntegrationOAuthRateLimit(t *testing.T) {
	e := newOAuthEnv(t, "auth_oauth_limit", auth.Options{IPLimiter: auth.NewLimiter(2, time.Minute, 100, clock.System{})})
	release, err := e.svc.ReserveOAuthAttempt(e.ctx, oauthIP)
	if err != nil {
		t.Fatal(err)
	}
	release()
	for i := 0; i < 2; i++ {
		if _, err := e.svc.ReserveOAuthAttempt(e.ctx, oauthIP); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.svc.ReserveOAuthAttempt(e.ctx, oauthIP); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("third failed attempt: %v", err)
	}
	// The password login of the same address shares the bucket.
	if _, err := e.svc.Login(e.ctx, auth.LoginInput{Login: "admin", Password: "admin password", IP: oauthIP}); !errors.Is(err, auth.ErrRateLimited) {
		t.Fatalf("password login after oauth failures: %v", err)
	}
}
