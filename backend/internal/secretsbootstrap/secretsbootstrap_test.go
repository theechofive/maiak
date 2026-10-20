package secretsbootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/theechofive/maiak/backend/internal/platform/config"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/testsupport/pgtest"
)

const ns = "maiak"

// cheap keeps the race detector fast where the default cost is not under test.
var cheap = crypto.Argon2Params{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

func opts(store SecretStore, log *slog.Logger) Options {
	return Options{
		Namespace:              ns,
		AdminMode:              ModeGenerate,
		CryptoMode:             ModeGenerate,
		AdminLogin:             "admin",
		AdminSecretName:        "maiak-admin",
		AdminInitialSecretName: "maiak-admin-initial",
		CryptoSecretName:       "maiak-crypto",
		CryptoKeyID:            "k1",
		Secrets:                store,
		Logger:                 log,
	}
}

func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func mustGet(t *testing.T, f *pgtest.FakeSecrets, name string) map[string][]byte {
	t.Helper()
	d, found, err := f.Get(context.Background(), ns, name)
	if err != nil || !found {
		t.Fatalf("secret %s: found=%v err=%v", name, found, err)
	}
	return d
}

// assertNoSecretMaterial fails if any secret value (or a recognisable encoding of it) is in the log.
func assertNoSecretMaterial(t *testing.T, logs string, f *pgtest.FakeSecrets) {
	t.Helper()
	var values []string
	for _, name := range []string{"maiak-admin", "maiak-admin-initial", "maiak-crypto"} {
		d, found, _ := f.Get(context.Background(), ns, name)
		if !found {
			continue
		}
		for k, v := range d {
			if k == KeyEmail || k == KeyLogin || k == KeyActiveKeyID {
				continue // not secret
			}
			values = append(values, string(v), base64.StdEncoding.EncodeToString(v))
		}
		if keys, ok := d[KeyKeys]; ok {
			for _, part := range strings.Split(string(keys), "\"") {
				if len(part) > 20 {
					values = append(values, part)
				}
			}
		}
	}
	if len(values) == 0 {
		t.Fatal("no secret values to look for")
	}
	for _, v := range values {
		if v != "" && strings.Contains(logs, v) {
			t.Fatalf("log contains secret material %q:\n%s", v, logs)
		}
	}
	if strings.Contains(logs, "$argon2id$") {
		t.Fatalf("log contains a password hash:\n%s", logs)
	}
}

func TestFreshGenerate(t *testing.T) {
	f := pgtest.NewFakeSecrets()
	log, buf := captureLog()
	res, err := Run(context.Background(), opts(f, log))
	if err != nil {
		t.Fatal(err)
	}
	if !res.AdminCreated || !res.AdminInitialCreated || !res.CryptoCreated {
		t.Fatalf("result = %+v", res)
	}

	initial := mustGet(t, f, "maiak-admin-initial")
	if len(initial) != 2 || string(initial[KeyLogin]) != "admin" {
		t.Fatalf("initial secret keys: %v", sortedKeys(initial))
	}
	pw := string(initial[KeyPassword])
	if raw, err := base64.RawURLEncoding.DecodeString(pw); err != nil || len(raw) != 24 || len(pw) != 32 {
		t.Fatalf("password is not 24 random bytes base64url (len %d): %v", len(pw), err)
	}
	if !strings.Contains(f.Annotations(ns, "maiak-admin-initial")[NoticeAnnotation], "delete this Secret") {
		t.Fatalf("initial secret lacks the notice annotation: %v", f.Annotations(ns, "maiak-admin-initial"))
	}

	admin := mustGet(t, f, "maiak-admin")
	if len(admin) != 2 || string(admin[KeyEmail]) != "admin" {
		t.Fatalf("admin secret keys: %v", sortedKeys(admin))
	}
	h, err := crypto.ParseArgon2idHash(string(admin[KeyPasswordHash]))
	if err != nil {
		t.Fatal(err)
	}
	if h.Params != crypto.DefaultArgon2Params {
		t.Fatalf("hash params %+v, want the hashpw defaults %+v", h.Params, crypto.DefaultArgon2Params)
	}
	// The same verifier the auth service uses at login.
	if ok, err := crypto.VerifyPassword(pw, string(admin[KeyPasswordHash])); err != nil || !ok {
		t.Fatalf("hash does not verify: %v %v", ok, err)
	}
	if ok, _ := crypto.VerifyPassword(pw+"x", string(admin[KeyPasswordHash])); ok {
		t.Fatal("wrong password verifies")
	}

	// The crypto Secret must load through the real config parser of the workloads.
	c := mustGet(t, f, "maiak-crypto")
	env := map[string]string{
		"MAIAK_DATABASE_URL":             "postgres://u:p@db.internal:5432/maiak",
		"MAIAK_PUBLIC_URL":               "https://maiak.example.com",
		"MAIAK_ENCRYPTION_KEYS":          string(c[KeyKeys]),
		"MAIAK_ENCRYPTION_ACTIVE_KEY_ID": string(c[KeyActiveKeyID]),
	}
	jobs, err := config.LoadJobs(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	if jobs.EncryptionKeyID != "k1" || len(jobs.EncryptionKeys["k1"]) != 32 {
		t.Fatalf("parsed keys: id=%q n=%d", jobs.EncryptionKeyID, len(jobs.EncryptionKeys))
	}
	if _, err := crypto.NewKeyring(jobs.EncryptionKeys, jobs.EncryptionKeyID); err != nil {
		t.Fatal(err)
	}

	assertNoSecretMaterial(t, buf.String(), f)
	for _, want := range []string{"maiak-admin-initial", "maiak-crypto", KeyPasswordHash} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestIdempotentRerun(t *testing.T) {
	f := pgtest.NewFakeSecrets()
	o := opts(f, nil)
	o.hashParams = &cheap
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	before := map[string]map[string][]byte{}
	for _, n := range []string{"maiak-admin", "maiak-admin-initial", "maiak-crypto"} {
		before[n] = mustGet(t, f, n)
	}
	creates := f.Creates

	log, buf := captureLog()
	o.Logger = log
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{}) || f.Creates != creates || f.Updates != 0 {
		t.Fatalf("second run changed something: %+v creates=%d updates=%d", res, f.Creates-creates, f.Updates)
	}
	for n, d := range before {
		got := mustGet(t, f, n)
		for k, v := range d {
			if !bytes.Equal(got[k], v) {
				t.Fatalf("%s/%s changed", n, k)
			}
		}
	}
	if !strings.Contains(buf.String(), "left untouched") {
		t.Fatalf("log: %s", buf.String())
	}
	assertNoSecretMaterial(t, buf.String(), f)

	// The owner deleted the initial Secret after logging in: it is not recreated.
	f.Delete(ns, "maiak-admin-initial")
	if res, err := Run(context.Background(), o); err != nil || res != (Result{}) {
		t.Fatalf("after deleting the initial secret: %+v %v", res, err)
	}
	if _, found, _ := f.Get(context.Background(), ns, "maiak-admin-initial"); found {
		t.Fatal("initial secret recreated")
	}
}

func TestIncompleteSecretFails(t *testing.T) {
	cases := []struct {
		secret string
		data   map[string][]byte
		want   string
	}{
		{"maiak-crypto", map[string][]byte{KeyKeys: []byte(`{"k1":"x"}`)}, "lacks key(s) active_key_id"},
		{"maiak-crypto", map[string][]byte{KeyKeys: {}, KeyActiveKeyID: []byte("k1")}, "lacks key(s) keys"},
		{"maiak-admin", map[string][]byte{KeyEmail: []byte("admin")}, "lacks key(s) password_hash"},
		{"maiak-admin-initial", map[string][]byte{KeyLogin: []byte("admin")}, "lacks key(s) password"},
	}
	for _, tc := range cases {
		t.Run(tc.secret+"/"+tc.want, func(t *testing.T) {
			f := pgtest.NewFakeSecrets()
			f.Put(ns, tc.secret, tc.data)
			o := opts(f, nil)
			o.hashParams = &cheap
			_, err := Run(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "refusing to guess") {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			got := mustGet(t, f, tc.secret)
			if len(got) != len(tc.data) {
				t.Fatalf("incomplete secret was modified: %v", sortedKeys(got))
			}
			if _, found, _ := f.Get(context.Background(), ns, "maiak-admin"); found && tc.secret == "maiak-admin-initial" {
				t.Fatal("admin secret created from an incomplete initial secret")
			}
		})
	}
}

func TestInitialWithoutAdminIsCompleted(t *testing.T) {
	f := pgtest.NewFakeSecrets()
	f.Put(ns, "maiak-admin-initial", map[string][]byte{KeyLogin: []byte("admin"), KeyPassword: []byte("left-by-a-crashed-run")})
	o := opts(f, nil)
	o.hashParams = &cheap
	res, err := Run(context.Background(), o)
	if err != nil || !res.AdminCreated || res.AdminInitialCreated {
		t.Fatalf("%+v %v", res, err)
	}
	if ok, _ := crypto.VerifyPassword("left-by-a-crashed-run", string(mustGet(t, f, "maiak-admin")[KeyPasswordHash])); !ok {
		t.Fatal("admin hash does not match the initial password")
	}

	f = pgtest.NewFakeSecrets()
	f.Put(ns, "maiak-admin-initial", map[string][]byte{KeyLogin: []byte("root"), KeyPassword: []byte("pw")})
	if _, err := Run(context.Background(), opts(f, nil)); err == nil || !strings.Contains(err.Error(), "different login") {
		t.Fatalf("err = %v", err)
	}
}

func TestSkipModes(t *testing.T) {
	f := pgtest.NewFakeSecrets()
	o := opts(f, nil)
	o.AdminMode, o.CryptoMode = ModeSkip, ModeSkip
	o.AdminLogin = "" // not needed when skipped
	if res, err := Run(context.Background(), o); err != nil || res != (Result{}) || f.Creates != 0 {
		t.Fatalf("%+v %v creates=%d", res, err, f.Creates)
	}

	o.CryptoMode = ModeGenerate
	if res, err := Run(context.Background(), o); err != nil || !res.CryptoCreated || res.AdminCreated || f.Creates != 1 {
		t.Fatalf("crypto only: %+v %v", res, err)
	}

	f = pgtest.NewFakeSecrets()
	o = opts(f, nil)
	o.hashParams = &cheap
	o.CryptoMode = ModeSkip
	if res, err := Run(context.Background(), o); err != nil || res.CryptoCreated || !res.AdminCreated || f.Creates != 2 {
		t.Fatalf("admin only: %+v %v", res, err)
	}

	o.AdminMode = "maybe"
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestBadLogin(t *testing.T) {
	for _, login := range []string{"", "Admin", "ad min", "admin\n", strings.Repeat("a", 65), "adm!n"} {
		f := pgtest.NewFakeSecrets()
		o := opts(f, nil)
		o.AdminLogin = login
		o.CryptoMode = ModeSkip
		if _, err := Run(context.Background(), o); err == nil {
			t.Fatalf("login %q accepted", login)
		}
		if f.Creates != 0 {
			t.Fatalf("login %q: %d secrets created", login, f.Creates)
		}
	}
	for _, login := range []string{"admin", "ops.team_1", "owner@example.com", strings.Repeat("a", 64)} {
		if err := ValidateLogin(login); err != nil {
			t.Fatalf("login %q rejected: %v", login, err)
		}
	}
}

// racyStore lets a "concurrent run" create the Secret between our Get and our Create.
type racyStore struct {
	*pgtest.FakeSecrets
	before func(name string)
}

func (r *racyStore) CreateAnnotated(ctx context.Context, ns, name string, data map[string][]byte, ann map[string]string) error {
	if r.before != nil {
		r.before(name)
	}
	return r.FakeSecrets.CreateAnnotated(ctx, ns, name, data, ann)
}

func TestCreateRace409(t *testing.T) {
	f := pgtest.NewFakeSecrets()
	winnerHash, err := crypto.HashPassword("winner-pw", cheap)
	if err != nil {
		t.Fatal(err)
	}
	once := map[string]bool{}
	r := &racyStore{FakeSecrets: f, before: func(name string) {
		if once[name] {
			return
		}
		once[name] = true
		switch name {
		case "maiak-crypto":
			f.Put(ns, name, map[string][]byte{KeyKeys: []byte(`{"k1":"winner"}`), KeyActiveKeyID: []byte("k1")})
		case "maiak-admin-initial":
			f.Put(ns, name, map[string][]byte{KeyLogin: []byte("admin"), KeyPassword: []byte("winner-pw")})
		case "maiak-admin":
			f.Put(ns, name, map[string][]byte{KeyEmail: []byte("admin"), KeyPasswordHash: []byte(winnerHash)})
		}
	}}
	o := opts(r, nil)
	o.hashParams = &cheap
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{}) {
		t.Fatalf("lost every race but reports creations: %+v", res)
	}
	if string(mustGet(t, f, "maiak-crypto")[KeyKeys]) != `{"k1":"winner"}` || string(mustGet(t, f, "maiak-admin")[KeyPasswordHash]) != winnerHash {
		t.Fatal("winner's secrets were overwritten")
	}

	// The race resolves to an incomplete Secret: fail, never overwrite.
	f = pgtest.NewFakeSecrets()
	r = &racyStore{FakeSecrets: f, before: func(name string) { f.Put(ns, name, map[string][]byte{"junk": []byte("x")}) }}
	o = opts(r, nil)
	o.AdminMode = ModeSkip
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "lacks key(s)") {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentRuns(t *testing.T) {
	f := pgtest.NewFakeSecrets()
	log, buf := captureLog()
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := opts(f, log)
			o.hashParams = &cheap
			_, err := Run(context.Background(), o)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.Creates != 3 {
		t.Fatalf("creates = %d, want 3", f.Creates)
	}
	pw := string(mustGet(t, f, "maiak-admin-initial")[KeyPassword])
	if ok, err := crypto.VerifyPassword(pw, string(mustGet(t, f, "maiak-admin")[KeyPasswordHash])); err != nil || !ok {
		t.Fatalf("admin hash does not match the initial password: %v %v", ok, err)
	}
	assertNoSecretMaterial(t, buf.String(), f)
}

func TestLoadConfig(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	c, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	o := c.Options
	if o.AdminMode != ModeGenerate || o.CryptoMode != ModeGenerate || o.AdminLogin != "admin" || c.Namespace != "" ||
		o.AdminSecretName != "maiak-admin" || o.AdminInitialSecretName != "maiak-admin-initial" ||
		o.CryptoSecretName != "maiak-crypto" || o.CryptoKeyID != "k1" {
		t.Fatalf("defaults: %+v", c)
	}

	_, err = LoadConfig(env(map[string]string{
		"MAIAK_SECRETS_ADMIN_MODE":                "always",
		"MAIAK_SECRETS_CRYPTO_MODE":               "never",
		"MAIAK_SECRETS_ADMIN_LOGIN":               "Bad Login",
		"MAIAK_SECRETS_NAMESPACE":                 "Not_A_Label",
		"MAIAK_SECRETS_ADMIN_SECRET_NAME":         "same",
		"MAIAK_SECRETS_ADMIN_INITIAL_SECRET_NAME": "same",
		"MAIAK_SECRETS_CRYPTO_KEY_ID":             "bad id",
	}))
	for _, want := range []string{"MAIAK_SECRETS_ADMIN_MODE", "MAIAK_SECRETS_CRYPTO_MODE", "MAIAK_SECRETS_NAMESPACE",
		"MAIAK_SECRETS_ADMIN_INITIAL_SECRET_NAME", "MAIAK_SECRETS_CRYPTO_KEY_ID"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %s", err, want)
		}
	}

	_, err = LoadConfig(env(map[string]string{"MAIAK_SECRETS_ADMIN_LOGIN": "Bad Login"}))
	if err == nil || !strings.Contains(err.Error(), "MAIAK_SECRETS_ADMIN_LOGIN") {
		t.Fatalf("err = %v", err)
	}
	c, err = LoadConfig(env(map[string]string{"MAIAK_SECRETS_ADMIN_MODE": "skip", "MAIAK_SECRETS_ADMIN_LOGIN": "Bad Login", "MAIAK_SECRETS_NAMESPACE": "maiak"}))
	if err != nil || c.Namespace != "maiak" {
		t.Fatalf("login must not be validated in skip mode: %v", err)
	}
}
