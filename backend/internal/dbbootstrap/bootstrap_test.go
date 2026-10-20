package dbbootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		pw, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		b, err := base64.RawURLEncoding.DecodeString(pw)
		if err != nil || len(b) != 32 {
			t.Fatalf("password %q is not 32 bytes of unpadded URL-safe base64", pw)
		}
		if seen[pw] {
			t.Fatal("duplicate password")
		}
		seen[pw] = true
	}
}

func TestRoleURLEscapesAndRoundTrips(t *testing.T) {
	pw := `p@ss:w/rd?#%&= '"`
	raw := RoleURL("maiak_api", pw, "db.example.internal", 5432, "maiak", "require")
	if !strings.HasPrefix(raw, "postgres://maiak_api:") || !strings.HasSuffix(raw, "@db.example.internal:5432/maiak?sslmode=require") {
		t.Fatalf("unexpected url shape: %s", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := u.User.Password()
	if got != pw || u.User.Username() != "maiak_api" || u.Query().Get("sslmode") != "require" || u.Path != "/maiak" {
		t.Fatalf("round trip failed: %+v", u)
	}
	if v6 := RoleURL("r", "p", "::1", 5432, "maiak", "disable"); !strings.Contains(v6, "@[::1]:5432/") {
		t.Fatalf("ipv6 host not bracketed: %s", v6)
	}
}

func TestSCRAMVerifierMatchesReference(t *testing.T) {
	// Reference computed independently with Python hashlib/hmac (RFC 7677 construction).
	salt := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	got, err := scramVerifier("pencil", salt)
	if err != nil {
		t.Fatal(err)
	}
	want := "SCRAM-SHA-256$4096:AQIDBAUGBwgJCgsMDQ4PEA==$B9Mb8TSkDsvpddTD0BDmSDpJAo08+gvK8zcSCGRjCZw=:0z4kjgndRyJQnA93HtYE376F1vMDI59QM1nDcPyu8Rw="
	if got != want {
		t.Fatalf("verifier = %s\nwant      %s", got, want)
	}
	for _, bad := range []string{"", "with space", "ünïcode"} {
		if _, err := scramVerifier(bad, salt); err == nil {
			t.Errorf("scramVerifier(%q) should fail", bad)
		}
	}
}

func TestSQLBuilders(t *testing.T) {
	owner := roles[0]
	create := createRoleSQL(owner)
	for _, want := range []string{
		"rolname = 'maiak_owner'",
		`CREATE ROLE "maiak_owner" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 3;`,
	} {
		if !strings.Contains(create, want) {
			t.Errorf("createRoleSQL missing %q:\n%s", want, create)
		}
	}
	alter := alterRoleSQL(owner)
	if alter != `ALTER ROLE "maiak_owner" LOGIN NOCREATEDB NOCREATEROLE CONNECTION LIMIT 3` {
		t.Errorf("alterRoleSQL = %s", alter)
	}
	// Only a real superuser may mention these, so they must never appear in ALTER ROLE.
	for _, attr := range []string{"SUPERUSER", "REPLICATION", "BYPASSRLS"} {
		if strings.Contains(alter, attr) {
			t.Errorf("alterRoleSQL must not contain %s", attr)
		}
	}
	if got := roleSettingSQL("maiak_api", "statement_timeout", "15s"); got != `ALTER ROLE "maiak_api" SET "statement_timeout" = '15s'` {
		t.Errorf("roleSettingSQL = %s", got)
	}
	if got := createDatabaseSQL("maiak"); got != `CREATE DATABASE "maiak" OWNER "maiak_owner" TEMPLATE template0 ENCODING 'UTF8'` {
		t.Errorf("createDatabaseSQL = %s", got)
	}
	grants := strings.Join(databaseGrantsSQL("maiak"), ";")
	if !strings.Contains(grants, `REVOKE ALL ON DATABASE "maiak" FROM PUBLIC`) ||
		!strings.Contains(grants, `GRANT CONNECT ON DATABASE "maiak" TO "maiak_owner", "maiak_api", "maiak_worker"`) {
		t.Errorf("databaseGrantsSQL = %s", grants)
	}
	if got := ident(`we"ird`); got != `"we""ird"` {
		t.Errorf("ident = %s", got)
	}
	if got := quoteLiteral(`it's`); got != `'it''s'` {
		t.Errorf("quoteLiteral = %s", got)
	}
	if !strings.Contains(passwordSQL, "%I") || !strings.Contains(passwordSQL, "%L") {
		t.Error("password statement must be quoted server-side with format(%I, %L)")
	}
}

func TestRoleSpecs(t *testing.T) {
	want := map[string]int{OwnerRole: 3, APIRole: 20, WorkerRole: 20}
	for _, r := range roles {
		if want[r.name] != r.connLimit {
			t.Errorf("%s connection limit = %d", r.name, r.connLimit)
		}
	}
}

func TestPasswordsFromRequiresAllKeys(t *testing.T) {
	full := map[string][]byte{KeyOwnerPassword: []byte("a"), KeyAPIPassword: []byte("b"), KeyWorkerPassword: []byte("c")}
	if pw, err := passwordsFrom(full); err != nil || pw[KeyAPIPassword] != "b" {
		t.Fatalf("pw=%v err=%v", pw, err)
	}
	partial := map[string][]byte{KeyOwnerPassword: []byte("a"), "api_url": []byte("x")}
	_, err := passwordsFrom(partial)
	if err == nil || !strings.Contains(err.Error(), "api_password") || !strings.Contains(err.Error(), "worker_password") {
		t.Fatalf("err = %v", err)
	}
}

func TestKubeSecretsClient(t *testing.T) {
	stored := map[string]map[string][]byte{}
	annotations := map[string]map[string]string{}
	var authHeaders []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		const prefix = "/api/v1/namespaces/maiak/secrets"
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix+"/"):
			name := strings.TrimPrefix(r.URL.Path, prefix+"/")
			data, ok := stored[name]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"kind":"Status","reason":"NotFound","message":"not found"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(kubeSecret{APIVersion: "v1", Kind: "Secret", Metadata: kubeMeta{Name: name}, Data: data})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, prefix+"/"):
			name := strings.TrimPrefix(r.URL.Path, prefix+"/")
			var patch kubeSecret
			if r.Header.Get("Content-Type") != "application/merge-patch+json" || json.NewDecoder(r.Body).Decode(&patch) != nil {
				w.WriteHeader(http.StatusUnsupportedMediaType)
				return
			}
			if _, ok := stored[name]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			for k, v := range patch.Data {
				stored[name][k] = v
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == prefix:
			var s kubeSecret
			if err := json.NewDecoder(r.Body).Decode(&s); err != nil || s.Kind != "Secret" || s.Type != "Opaque" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if _, ok := stored[s.Metadata.Name]; ok {
				w.WriteHeader(http.StatusConflict)
				return
			}
			stored[s.Metadata.Name] = s.Data
			annotations[s.Metadata.Name] = s.Metadata.Annotations
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"kind":"Status","reason":"Forbidden","message":"nope"}`)
		}
	}))
	defer srv.Close()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("tok123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k := newKubeSecrets(srv.URL, tokenPath, srv.Client())
	ctx := context.Background()

	if _, found, err := k.Get(ctx, "maiak", "maiak-db"); err != nil || found {
		t.Fatalf("get missing: found=%v err=%v", found, err)
	}
	data := map[string][]byte{KeyAPIPassword: []byte("secret value")}
	if err := k.Create(ctx, "maiak", "maiak-db", data); err != nil {
		t.Fatal(err)
	}
	if err := k.Create(ctx, "maiak", "maiak-db", data); err != ErrSecretExists {
		t.Fatalf("second create: %v", err)
	}
	if err := k.CreateAnnotated(ctx, "maiak", "annotated", data, map[string]string{"maiak/notice": "hi"}); err != nil {
		t.Fatal(err)
	}
	if annotations["annotated"]["maiak/notice"] != "hi" || annotations["maiak-db"] != nil {
		t.Fatalf("annotations: %v", annotations)
	}
	got, found, err := k.Get(ctx, "maiak", "maiak-db")
	if err != nil || !found || string(got[KeyAPIPassword]) != "secret value" {
		t.Fatalf("get: %v %v %v", got, found, err)
	}
	if err := k.Update(ctx, "maiak", "maiak-db", map[string][]byte{KeyAPIURL: []byte("postgres://new")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _, _ = k.Get(ctx, "maiak", "maiak-db")
	if string(got[KeyAPIURL]) != "postgres://new" || string(got[KeyAPIPassword]) != "secret value" {
		t.Fatalf("update must merge keys: %v", got)
	}
	if err := k.Update(ctx, "maiak", "missing", map[string][]byte{KeyAPIURL: nil}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("update missing secret: %v", err)
	}
	if _, _, err := k.Get(ctx, "other", "x"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("forbidden: %v", err)
	}
	for _, h := range authHeaders {
		if h != "Bearer tok123" {
			t.Fatalf("authorization header = %q", h)
		}
	}
}

func TestURLDrift(t *testing.T) {
	o := &Options{Host: "db.internal", Port: 5432, DBName: "maiak", SSLMode: "require"}
	pw := map[string]string{KeyOwnerPassword: "o", KeyAPIPassword: "a", KeyWorkerPassword: "w"}
	data := map[string][]byte{
		KeyOwnerURL: []byte(RoleURL(OwnerRole, "o", "db.internal", 5432, "maiak", "require")),
		KeyAPIURL:   []byte(RoleURL(APIRole, "a", "old.internal", 5432, "maiak", "require")),
	}
	drift := o.urlDrift(data, pw)
	if len(drift) != 2 || drift[KeyOwnerURL] != nil {
		t.Fatalf("drift keys: %v", drift)
	}
	if string(drift[KeyAPIURL]) != RoleURL(APIRole, "a", "db.internal", 5432, "maiak", "require") ||
		string(drift[KeyWorkerURL]) != RoleURL(WorkerRole, "w", "db.internal", 5432, "maiak", "require") {
		t.Fatalf("drift values: %q", drift)
	}
	data[KeyAPIURL], data[KeyWorkerURL] = drift[KeyAPIURL], drift[KeyWorkerURL]
	if d := o.urlDrift(data, pw); len(d) != 0 {
		t.Fatalf("no drift expected: %v", d)
	}
}
