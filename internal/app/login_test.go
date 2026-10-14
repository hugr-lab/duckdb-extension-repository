package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/config"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// RDS IAM authentication (spec 0020): a presigned rds-db:connect for the connection's host, port and
// user, signed locally with the configured identity, good for 15 minutes.
func TestRDSLogin(t *testing.T) {
	dir := t.TempDir()
	ak, sk := filepath.Join(dir, "ak"), filepath.Join(dir, "sk")
	if err := os.WriteFile(ak, []byte("AKIAEXAMPLEEXAMPLE00\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sk, []byte("wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Profile = config.ProfileDev
	cfg.AWS = config.AWS{Region: "eu-central-1", Identity: &config.Identity{Kind: "static", AccessKeyFile: ak, SecretKeyFile: sk}}
	l, err := rdsLogin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tl, ok := l.(store.TokenLogin)
	if !ok {
		t.Fatalf("%T is not a token login", l)
	}
	tok, err := tl.PasswordFor(context.Background(), store.Target{Host: "db.abc.eu-central-1.rds.amazonaws.com", Port: 5432, User: "kista"})
	if err != nil {
		t.Fatal(err)
	}
	host, query, ok := strings.Cut(tok, "?")
	if !ok || host != "db.abc.eu-central-1.rds.amazonaws.com:5432" {
		t.Fatalf("token %q", tok)
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("Action") != "connect" || q.Get("DBUser") != "kista" || q.Get("X-Amz-Expires") != "900" ||
		!strings.Contains(q.Get("X-Amz-Credential"), "/eu-central-1/rds-db/aws4_request") || q.Get("X-Amz-Signature") == "" {
		t.Fatalf("token query %v", q)
	}
}

// Cloud SQL IAM authentication against the development fake: a fixed token, no credential loaded.
func TestCloudSQLLoginFake(t *testing.T) {
	cfg := config.Default()
	cfg.Profile = config.ProfileDev
	cfg.GCP = config.GCP{GCPCreds: config.GCPCreds{EndpointURL: "http://127.0.0.1:1"}}
	l, err := cloudSQLLogin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := l.(store.TokenLogin).PasswordFor(context.Background(), store.Target{Host: "h", Port: 5432, User: "u"})
	if err != nil || tok != "kista-dev-fake" {
		t.Fatalf("%q %v", tok, err)
	}
}

// The gcp login asks the metadata server for sqlservice.login only, never cloud-platform.
func TestCloudSQLLoginScope(t *testing.T) {
	const principal = "kista-db@acme-kista.iam.gserviceaccount.com"
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
	metadataURL = srv.URL + "/sa/"
	defer func() { metadataURL = "" }()
	cfg := config.Default()
	cfg.Profile = config.ProfileDev // no steering check against the test's environment
	cfg.GCP = config.GCP{GCPCreds: config.GCPCreds{Principal: principal}}
	l, err := cloudSQLLogin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := l.(store.TokenLogin).PasswordFor(context.Background(), store.Target{Host: "h", Port: 5432, User: "kista-db@acme-kista.iam"})
	if err != nil || tok != "ya29.sql" {
		t.Fatalf("%q %v", tok, err)
	}
	if asked != "https://www.googleapis.com/auth/sqlservice.login" {
		t.Fatalf("scopes %q", asked)
	}
}
