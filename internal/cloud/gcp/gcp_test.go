package gcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2/google/externalaccount"
)

const principal = "kista-signer@kista-test-1.iam.gserviceaccount.com"

func env(vars map[string]string, files map[string]string) Env {
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no file")
	}
	return Env{
		Environ: func() []string {
			var out []string
			for k, v := range vars {
				out = append(out, k+"="+v)
			}
			return out
		},
		ReadFile: read, ReadToken: read,
	}
}

// external is a valid external_account file, changed by mut.
func external(mut func(m map[string]any)) string {
	m := map[string]any{
		"type":               "external_account",
		"audience":           "//iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/kista-pool/providers/aks-1",
		"subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
		"token_url":          "https://sts.googleapis.com/v1/token",
		"service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + principal +
			":generateAccessToken",
		"credential_source": map[string]any{"file": "/var/run/secrets/tokens/gcp-token"},
		"universe_domain":   "googleapis.com",
	}
	if mut != nil {
		mut(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// What an external_account file may be: a projected token's federation impersonating the
// principal, and nothing else.
func TestExternalAccountRules(t *testing.T) {
	cs := func(v map[string]any) func(m map[string]any) {
		return func(m map[string]any) { m["credential_source"] = v }
	}
	cases := map[string]struct {
		file string
		want string // "" accepted
	}{
		"ok":            {external(nil), ""},
		"json format":   {external(cs(map[string]any{"file": "/t", "format": map[string]any{"type": "json", "subject_token_field_name": "token"}})), ""},
		"aws1":          {external(cs(map[string]any{"environment_id": "aws1", "regional_cred_verification_url": "https://sts.{region}.amazonaws.com?Action=GetCallerIdentity&Version=2011-06-15"})), "node's identity"},
		"azure url":     {external(cs(map[string]any{"url": "http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=x"})), "node's identity"},
		"executable":    {external(cs(map[string]any{"executable": map[string]any{"command": "/bin/sh -c id"}})), "executable"},
		"relative file": {external(cs(map[string]any{"file": "token"})), "absolute"},
		"json no field": {external(cs(map[string]any{"file": "/t", "format": map[string]any{"type": "json"}})), "format"},
		"unknown key":   {external(func(m map[string]any) { m["extra"] = 1 }), "does not accept"},
		"other principal": {external(func(m map[string]any) {
			m["service_account_impersonation_url"] = strings.Replace(m["service_account_impersonation_url"].(string), "kista-signer@", "other-signer@", 1)
		}), "principal"},
		"no impersonation": {external(func(m map[string]any) { delete(m, "service_account_impersonation_url") }), "principal"},
		"evil token url":   {external(func(m map[string]any) { m["token_url"] = "https://sts.evil.example/v1/token" }), "token_url"},
		"workforce pool": {external(func(m map[string]any) {
			m["audience"] = "//iam.googleapis.com/locations/global/workforcePools/p/providers/x"
		}), "audience"},
		"saml":           {external(func(m map[string]any) { m["subject_token_type"] = "urn:ietf:params:oauth:token-type:saml2" }), "subject_token_type"},
		"client secret":  {external(func(m map[string]any) { m["client_secret"] = "x" }), "people"},
		"other universe": {external(func(m map[string]any) { m["universe_domain"] = "example.com" }), "universe_domain"},
		"long lifetime": {external(func(m map[string]any) {
			m["service_account_impersonation"] = map[string]any{"token_lifetime_seconds": 43200}
		}), "600..3600"},
		"authorized user": {`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`, "not accepted"},
		"impersonated":    {`{"type":"impersonated_service_account"}`, "not accepted"},
		"service account": {`{"type":"service_account","token_uri":"https://oauth2.googleapis.com/token"}`, "profile dev"},
		"not json":        {`x`, "not JSON"},
	}
	for name, c := range cases {
		_, err := TokenSource("/creds.json", principal, Options{Env: env(nil, map[string]string{"/creds.json": c.file})})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

// Steering variables are refused outside dev; the principal is required there.
func TestSteeringAndPrincipal(t *testing.T) {
	for _, v := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GCE_METADATA_HOST", "STORAGE_EMULATOR_HOST", "CLOUDSDK_CONFIG"} {
		if _, err := TokenSource("", principal, Options{Env: env(map[string]string{v: "x"}, nil)}); err == nil || !strings.Contains(err.Error(), v) {
			t.Errorf("%s: %v", v, err)
		}
	}
	if _, err := TokenSource("", principal, Options{Env: env(map[string]string{"GOOGLE_CLOUD_UNIVERSE_DOMAIN": "googleapis.com",
		"GOOGLE_CLOUD_PROJECT": "p"}, nil)}); err != nil {
		t.Errorf("the default universe and a project: %v", err)
	}
	if _, err := TokenSource("", "", Options{Env: env(nil, nil)}); err == nil {
		t.Error("no principal outside dev")
	}
	if _, err := TokenSource("", "", Options{Dev: true, Env: env(map[string]string{"GCE_METADATA_HOST": "x"}, nil)}); err != nil {
		t.Errorf("dev: %v", err)
	}
}

// The metadata server: the account must be the principal; then its token is used, and the
// response must come from the metadata server.
func TestMetadataServer(t *testing.T) {
	email := principal
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		calls[r.URL.Path]++
		w.Header().Set("Metadata-Flavor", "Google")
		switch {
		case strings.HasSuffix(r.URL.Path, "/email"):
			w.Write([]byte(email))
		case strings.HasSuffix(r.URL.Path, "/token"):
			if !strings.Contains(r.URL.RawQuery, "cloud-platform") {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"access_token":"ya29.x","expires_in":3599,"token_type":"Bearer"}`))
		}
	}))
	defer srv.Close()
	ts, err := TokenSource("", principal, Options{Env: env(nil, nil), MetadataURL: srv.URL + "/sa/"})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		tok, err := ts.Token()
		if err != nil || tok.AccessToken != "ya29.x" {
			t.Fatalf("%+v %v", tok, err)
		}
	}
	if calls["/sa/token"] != 1 || calls["/sa/email"] != 1 {
		t.Fatalf("calls %v (the token cached, the account checked once)", calls)
	}
	email = "node@kista-test-1.iam.gserviceaccount.com"
	ts, _ = TokenSource("", principal, Options{Env: env(nil, nil), MetadataURL: srv.URL + "/sa/"})
	if _, err := ts.Token(); err == nil || !strings.Contains(err.Error(), "not the principal") {
		t.Fatalf("the node's account: %v", err)
	}
}

// A projected token: text or a JSON field, never empty; read through Kubernetes' symlinks.
func TestProjectedToken(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, "..2026")
	if err := os.Mkdir(stamp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stamp, "token"), []byte("eyJ.a.b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token")
	if err := os.Symlink("..data/token", link); err != nil {
		t.Fatal(err)
	}
	if tok, err := (projectedToken{path: link, read: readTokenFile}).SubjectToken(t.Context(), externalaccount.SupplierOptions{}); err != nil || tok != "eyJ.a.b" {
		t.Fatalf("%q %v", tok, err)
	}
}

// A service_account key in dev must be the principal's and use Google's token endpoint; a
// credentials file nobody else may write; quota_project_id is not taken.
func TestFilesAndKeys(t *testing.T) {
	sa := func(email, uri string) string {
		b, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": email, "token_uri": uri,
			"private_key": "-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----\n"})
		return string(b)
	}
	for name, c := range map[string]struct{ file, want string }{
		"other principal": {sa("other-signer@kista-test-1.iam.gserviceaccount.com", "https://oauth2.googleapis.com/token"), "principal"},
		"evil token uri":  {sa(principal, "https://evil.example/token"), "token_uri"},
		"quota project":   {external(func(m map[string]any) { m["quota_project_id"] = "p" }), "quota_project_id"},
	} {
		_, err := TokenSource("/c.json", principal, Options{Dev: true, Env: env(nil, map[string]string{"/c.json": c.file})})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "creds.json")
	if err := os.WriteFile(p, []byte(external(nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o664); err != nil { // past the umask
		t.Fatal(err)
	}
	if _, err := readCredentialsFile(p); err == nil {
		t.Error("a group-writable credentials file was read")
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredentialsFile(p); err != nil {
		t.Errorf("a 0644 file of kista's own: %v", err)
	}
}

// The database login's token (spec 0020): asked for sqlservice.login only, never cloud-platform.
func TestMetadataScopes(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		switch {
		case strings.HasSuffix(r.URL.Path, "/email"):
			w.Write([]byte(principal))
		case strings.HasSuffix(r.URL.Path, "/token"):
			asked = r.URL.Query().Get("scopes")
			w.Write([]byte(`{"access_token":"ya29.sql","expires_in":3599,"token_type":"Bearer"}`))
		}
	}))
	defer srv.Close()
	ts, err := TokenSource("", principal, Options{Env: env(nil, nil), MetadataURL: srv.URL + "/sa/", Scopes: []string{SQLLoginScope}})
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := ts.Token(); err != nil || tok.AccessToken != "ya29.sql" {
		t.Fatalf("%+v %v", tok, err)
	}
	if asked != SQLLoginScope {
		t.Fatalf("scopes %q", asked)
	}
}
