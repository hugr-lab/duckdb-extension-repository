package e2e

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Tier B: an http:// prefix in a process where httpfs was never loaded, so DuckDB's built-in client
// downloads. Tier C: httpfs, installed and loaded from our own repository first.

func (b *build) flatPath(name string) string {
	return "/" + b.versionDir + "/" + b.platform + "/" + name + ".duckdb_extension"
}

// Cases 9 and 10: the bootstrap. Over http:// with USING PUBLIC KEY, INSTALL httpfs makes exactly one
// GET of the .gz (no HEAD, no plain-name fallback); the .gz is kista's assembled stream (precompressed
// body + stored signature block), which DuckDB inflates and verifies.
func TestHTTPBootstrap(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.add("httpfs", k, "")
	srv := newFileServer(t, r.dir, serverOpts{})

	res := newSession(t, b, nil).exec(
		createRepo("r", srv.URL(), k.pem),
		"INSTALL httpfs FROM r",
		"LOAD httpfs FROM r",
		"SELECT loaded FROM duckdb_extensions() WHERE extension_name = 'httpfs'",
	)
	mustOK(t, res)
	if res[3].Rows[0][0] != "true" {
		t.Fatalf("httpfs not loaded: %v", res[3].Rows)
	}
	log := srv.requests()
	if len(log) != 1 || log[0].Method != "GET" || log[0].Path != b.flatPath("httpfs")+".gz" {
		t.Fatalf("requests: %+v", log)
	}
}

// Case 11: the built-in client never sends Authorization, even with an http secret whose scope
// matches.
func TestHTTPNoAuthorizationOnBuiltinClient(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.add("loadable_extension_demo", k, "")
	srv := newFileServer(t, r.dir, serverOpts{})
	mustOK(t, newSession(t, b, nil).exec(
		createRepo("r", srv.URL(), k.pem),
		"CREATE SECRET s (TYPE http, BEARER_TOKEN 'tok', SCOPE "+sqlString(srv.URL()+"/")+")",
		"INSTALL loadable_extension_demo FROM r",
	))
	for _, q := range srv.requests() {
		if q.Auth != "" {
			t.Fatalf("Authorization sent by the built-in client: %+v", q)
		}
	}
	// and a repository that requires a token cannot be used on this path
	tsrv := newFileServer(t, r.dir, serverOpts{token: "tok"})
	res := newSession(t, b, nil).exec(
		createRepo("r", tsrv.URL(), k.pem),
		"CREATE SECRET s (TYPE http, BEARER_TOKEN 'tok', SCOPE "+sqlString(tsrv.URL()+"/")+")",
		"INSTALL loadable_extension_demo FROM r",
	)
	mustOK(t, res[:2])
	if res[2].OK {
		t.Fatal("installed from a token-protected repository without a token")
	}
}

// Case 12: a repeated INSTALL makes no request. UPDATE EXTENSIONS does not see installs from a
// user-provided repository at all; on a core-typed flat install from an http URL it sends
// If-None-Match with the served ETag, and a 304 keeps the install. The second part checks transport
// only, so it runs with allow_unsigned_extensions: the binary is not DuckDB-signed.
func TestHTTPUpdateUsesETag(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.add("loadable_extension_demo", k, "")
	srv := newFileServer(t, r.dir, serverOpts{})
	opts := map[string]string{"extension_directory": t.TempDir()}
	mustOK(t, newSession(t, b, opts).exec(
		createRepo("r", srv.URL(), k.pem),
		"INSTALL loadable_extension_demo FROM r",
		"INSTALL loadable_extension_demo FROM r",
	))
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("%d requests for two INSTALLs, want 1", n)
	}
	res := newSession(t, b, opts).exec(
		createRepo("r", srv.URL(), k.pem),
		"UPDATE EXTENSIONS (loadable_extension_demo)",
	)
	mustOK(t, res[:1])
	mustFail(t, res[1], "not installed")

	core := map[string]string{"extension_directory": t.TempDir(), "allow_unsigned_extensions": "true"}
	mustOK(t, newSession(t, b, core).exec("INSTALL loadable_extension_demo FROM "+sqlString(srv.URL())))
	before := len(srv.requests())
	res = newSession(t, b, core).exec(
		"UPDATE EXTENSIONS (loadable_extension_demo)",
		"LOAD loadable_extension_demo",
		"SELECT test_alias_hello()",
	)
	mustOK(t, res)
	log := srv.requests()[before:]
	if len(log) != 1 || log[0].IfNoneMatch == "" || log[0].Status != 304 {
		t.Fatalf("UPDATE EXTENSIONS requests: %+v", log)
	}
	t.Logf("UPDATE EXTENSIONS result: %v", res[0].Rows)
}

// Case 13: without httpfs, CREATE over http:// cannot read .well-known: a key must be given.
func TestHTTPWellKnownNeedsHTTPFS(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.wellKnown(k)
	srv := newFileServer(t, r.dir, serverOpts{})
	res := newSession(t, b, nil).exec(createRepo("r", srv.URL()))
	if res[0].OK {
		t.Fatal("CREATE read .well-known without httpfs")
	}
	t.Logf("CREATE without a key, no httpfs: %s", res[0].Error)
	if len(srv.requests()) != 0 {
		t.Fatalf("requests: %+v", srv.requests())
	}
}

// bootstrapHTTPFS returns statements that install and load httpfs from a plain-http repository
// signed with k (the route for a DuckDB version nobody publishes binaries for), then trust the test
// CA for https downloads. httpfs verifies server certificates by default.
func bootstrapHTTPFS(t *testing.T, b *build, k *key) []string {
	r := newRepo(t, b)
	r.add("httpfs", k, "")
	srv := newFileServer(t, r.dir, serverOpts{})
	return []string{
		createRepo("boot", srv.URL(), k.pem),
		"INSTALL httpfs FROM boot",
		"LOAD httpfs FROM boot",
		"SET ca_cert_file = " + sqlString(testCA(t).caFile),
	}
}

const bootstrapLen = 4

// Case 14: with httpfs loaded, CREATE reads the keys from .well-known (a trailing / on the prefix is
// trimmed), and a .well-known over 64 KiB is refused. The repository is plain http: .well-known is
// read through httpfs's file system, which is not bumped to https (only extension downloads are), and
// is read without ca_cert_file (case 18), so a private-CA https server cannot serve it.
func TestHTTPSWellKnown(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.wellKnown(k)
	r.add("loadable_extension_demo", k, "")
	big := newRepo(t, b)
	// a .well-known of exactly 64 KiB is accepted and one byte more is refused; pad with spaces
	// after the JSON, which DuckDB's parser ignores
	data, _ := json.Marshal(struct {
		SignatureKeys []string `json:"signature_keys"`
	}{[]string{k.pem}})
	exact := newRepo(t, b)
	exact.write(".well-known/duckdb-extension-repo.json", append(data, []byte(strings.Repeat(" ", 64<<10-len(data)))...))
	big.write(".well-known/duckdb-extension-repo.json", append(data, []byte(strings.Repeat(" ", 64<<10-len(data)+1))...))
	exactSrv := newFileServer(t, exact.dir, serverOpts{})

	srv := newFileServer(t, r.dir, serverOpts{})
	bigSrv := newFileServer(t, big.dir, serverOpts{})
	stmts := append(bootstrapHTTPFS(t, b, k),
		createRepo("r", srv.URL()+"/"),
		"SELECT prefix, unnest(key_fingerprints) FROM duckdb_extension_repositories() WHERE repository_name = 'r'",
		createRepo("big", bigSrv.URL()),
		createRepo("exact", exactSrv.URL()),
	)
	res := newSession(t, b, nil).exec(stmts...)
	n := bootstrapLen
	mustOK(t, res[:n+2])
	mustOK(t, res[n+3:])
	if got := res[n+1].Rows[0]; got[1] != k.fp {
		t.Fatalf("repository row %v, want %s", got, k.fp)
	}
	t.Logf("prefix as stored: %v", res[n+1].Rows[0][0])
	mustFail(t, res[n+2], "too large")
	var wk bool
	for _, q := range srv.requests() {
		wk = wk || strings.HasSuffix(q.Path, "/.well-known/duckdb-extension-repo.json")
		if strings.Contains(q.Path, "//") {
			t.Fatalf("trailing slash not trimmed: %s", q.Path)
		}
	}
	if !wk {
		t.Fatal(".well-known was not requested")
	}
}

// Case 15: the https install path, through httpfs: the request sequence.
func TestHTTPSRequestSequence(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.add("loadable_extension_demo", k, "")
	srv := newFileServer(t, r.dir, serverOpts{tls: true})
	stmts := append(bootstrapHTTPFS(t, b, k),
		createRepo("r", srv.URL(), k.pem),
		"INSTALL loadable_extension_demo FROM r",
	)
	mustOK(t, newSession(t, b, nil).exec(stmts...))
	for _, q := range srv.requests() {
		t.Logf("%s %s range=%q status=%d", q.Method, q.Path, q.Range, q.Status)
	}
	// HEAD on the .gz, then one ranged GET of it; never the plain name when the .gz exists
	gz := b.flatPath("loadable_extension_demo") + ".gz"
	log := srv.requests()
	if len(log) != 2 || log[0].Method != "HEAD" || log[0].Path != gz ||
		log[1].Method != "GET" || log[1].Path != gz || log[1].Range == "" || log[1].Status != 206 {
		t.Fatalf("requests: %+v", log)
	}
}

// Cases 16 and 17: an http secret's SCOPE is a plain string prefix. With a trailing slash the token
// goes only under the prefix; without it, also to the sibling …/prod2/.
func TestHTTPSSecretScope(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	root := newRepo(t, b)
	prod := &repo{t: t, b: b, dir: root.dir + "/acme/prod"}
	prod2 := &repo{t: t, b: b, dir: root.dir + "/acme/prod2"}
	prod.add("loadable_extension_demo", k, "")
	prod2.add("demo_capi", k, "")
	srv := newFileServer(t, root.dir, serverOpts{tls: true})

	for _, slash := range []bool{true, false} {
		before := len(srv.requests())
		scope := srv.URL() + "/acme/prod"
		if slash {
			scope += "/"
		}
		stmts := append(bootstrapHTTPFS(t, b, k),
			"CREATE SECRET s (TYPE http, BEARER_TOKEN 'tok', SCOPE "+sqlString(scope)+")",
			createRepo("prod", srv.URL()+"/acme/prod", k.pem),
			createRepo("prod2", srv.URL()+"/acme/prod2", k.pem),
			"INSTALL loadable_extension_demo FROM prod",
			"INSTALL demo_capi FROM prod2",
		)
		mustOK(t, newSession(t, b, nil).exec(stmts...))
		var prodAuth, prod2Auth bool
		for _, q := range srv.requests()[before:] {
			switch {
			case strings.HasPrefix(q.Path, "/acme/prod/"):
				prodAuth = prodAuth || q.Auth == "Bearer tok"
			case strings.HasPrefix(q.Path, "/acme/prod2/"):
				prod2Auth = prod2Auth || q.Auth != ""
			}
		}
		if !prodAuth {
			t.Fatalf("slash=%v: no Bearer under the prefix", slash)
		}
		if slash && prod2Auth {
			t.Fatal("scope with a trailing slash sent the token to …/prod2/")
		}
		if !slash && !prod2Auth {
			t.Fatal("scope without a trailing slash did not reach …/prod2/: the rule's reason is gone")
		}
	}
}

// Case 18: a private CA. ca_cert_file applies to installs, but .well-known is read without the
// statement's context.
func TestHTTPSPrivateCA(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.wellKnown(k)
	r.add("loadable_extension_demo", k, "")
	srv := newFileServer(t, r.dir, serverOpts{tls: true})
	ca := testCA(t).caFile
	stmts := append(bootstrapHTTPFS(t, b, k),
		"SET enable_server_cert_verification = true",
		"SET ca_cert_file = "+sqlString(ca),
		createRepo("nokey", srv.URL()),
		createRepo("r", srv.URL(), k.pem),
		"INSTALL loadable_extension_demo FROM r",
	)
	res := newSession(t, b, nil).exec(stmts...)
	n := bootstrapLen + 2
	mustOK(t, res[:n])
	// .well-known is read without ca_cert_file: the certificate is not trusted
	mustFail(t, res[n], "certificate")
	mustOK(t, res[n+1:])

	// negative control: without ca_cert_file the install fails on the certificate, so the
	// successful install above did use ca_cert_file
	boot := bootstrapHTTPFS(t, b, k)[:bootstrapLen-1]
	res = newSession(t, b, nil).exec(append(boot,
		createRepo("r", srv.URL(), k.pem),
		"INSTALL loadable_extension_demo FROM r",
	)...)
	mustOK(t, res[:len(boot)+1])
	if res[len(boot)+1].OK {
		t.Fatal("install over a private CA worked without ca_cert_file")
	}
}

// Case 19: autoload is core-only. With autoinstall on and custom_extension_repository pointing at our
// repository, a query that needs httpfs fetches our httpfs but refuses its signature; httpfs stays
// unloaded (autoload swallows the error, so the state is asserted, not a message).
func TestAutoloadIsCoreOnly(t *testing.T) {
	b := needBuild(t)
	k := newKey(t)
	r := newRepo(t, b)
	r.add("httpfs", k, "")
	srv := newFileServer(t, r.dir, serverOpts{})
	extDir := t.TempDir()
	res := newSession(t, b, map[string]string{
		"extension_directory":          extDir,
		"autoinstall_known_extensions": "true",
		"autoload_known_extensions":    "true",
		"custom_extension_repository":  srv.URL(),
	}).exec(
		// fails either way (nothing listens on port 9); what matters is whether httpfs got loaded
		"SELECT * FROM read_text('https://127.0.0.1:9/x.txt')",
		"SELECT loaded FROM duckdb_extensions() WHERE extension_name = 'httpfs'",
	)
	mustOK(t, res[1:])
	if installed, _ := filepath.Glob(filepath.Join(extDir, "*", "*", "httpfs.duckdb_extension")); len(installed) != 0 {
		t.Fatalf("httpfs was installed: %v", installed)
	}
	if res[1].Rows[0][0] != "false" {
		t.Fatalf("httpfs loaded: %v", res[1].Rows)
	}
	var fetched bool
	for _, q := range srv.requests() {
		fetched = fetched || strings.Contains(q.Path, "httpfs")
	}
	if !fetched {
		t.Fatal("autoinstall never asked our repository: the case proves nothing")
	}
}
