package e2e

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keys"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/tenants"
)

// Spec 0003: a channel and its keys from the store, .well-known from keys.Service.WellKnown, read by
// DuckDB. A local-directory prefix reads .well-known through DuckDB's local file system, so this
// runs without httpfs (tier A).
func TestStoreWellKnownAndRotation(t *testing.T) {
	b := needBuild(t)
	ctx := context.Background()
	admin := authz.Actor{Kind: authz.ActorOS, ID: "e2e"}

	st, err := store.OpenSQLite(ctx, filepath.Join(t.TempDir(), "kista.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	keyDir := t.TempDir()
	ten := &tenants.Service{Store: st, Authz: authz.ServerAdmin{}, HasDomain: func(d string) bool { return d == "default" }}
	ks := &keys.Service{Store: st, Signers: signer.Resolver{FileDir: keyDir, AllowFile: true}, Authz: authz.ServerAdmin{}}
	if _, err := ten.CreateTenant(ctx, admin, "acme", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ten.CreateChannel(ctx, admin, "acme", "prod", store.ChannelSigned); err != nil {
		t.Fatal(err)
	}
	addKey := func(name string, active bool) (store.Key, *key) {
		t.Helper()
		if err := signer.GenerateKeyFile(filepath.Join(keyDir, name)); err != nil {
			t.Fatal(err)
		}
		k, err := ks.Add(ctx, admin, "acme", "prod", "file:"+name, active)
		if err != nil {
			t.Fatal(err)
		}
		sg, err := ks.OpenSigner(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		return k, keyFrom(t, sg)
	}
	// publish writes the store's .well-known into the repository tree
	publish := func(r *repo) {
		t.Helper()
		doc, _, err := ks.WellKnown(ctx, admin, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		r.write(".well-known/duckdb-extension-repo.json", doc)
	}
	// fingerprints are what DuckDB pinned for the repository
	fingerprints := func(res []result) []string {
		var out []string
		for _, row := range res[len(res)-1].Rows {
			out = append(out, row[0].(string))
		}
		sort.Strings(out)
		return out
	}
	storeFPs := func() []string {
		t.Helper()
		list, err := ks.List(ctx, admin, "acme", "prod")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, k := range list {
			if k.State != store.KeyRetired {
				out = append(out, k.Fingerprint)
			}
		}
		sort.Strings(out)
		return out
	}
	pinned := "SELECT unnest(key_fingerprints) FROM duckdb_extension_repositories() WHERE repository_name = 'r'"

	// key A, active from the start; the repository serves an A-signed extension
	_, keyA := addKey("a.pem", true)
	ra := newRepo(t, b)
	ra.add("loadable_extension_demo", keyA, "")
	publish(ra)
	extDir := t.TempDir()
	opts := map[string]string{"extension_directory": extDir}
	res := newSession(t, b, opts).exec(
		createRepo("r", ra.dir), // no key: .well-known
		"INSTALL loadable_extension_demo FROM r",
		"LOAD loadable_extension_demo FROM r",
		pinned,
	)
	mustOK(t, res)
	if got, want := strings.Join(fingerprints(res), ","), strings.Join(storeFPs(), ","); got != want {
		t.Fatalf("DuckDB pinned %s, the store has %s", got, want)
	}

	// key B added and activated: .well-known lists both, and the A-signed install still loads
	kB, keyB := addKey("b.pem", false)
	if _, err := ks.Activate(ctx, admin, "acme", "prod", kB.ID, false); err != nil {
		t.Fatal(err)
	}
	publish(ra)
	res = newSession(t, b, opts).exec(createRepo("r", ra.dir), "LOAD loadable_extension_demo FROM r", pinned)
	mustOK(t, res)
	if got := fingerprints(res); len(got) != 2 || strings.Join(got, ",") != strings.Join(storeFPs(), ",") {
		t.Fatalf("after activation DuckDB pinned %v", got)
	}

	// A retired: no longer trusted, so the A-signed install fails to load until FORCE INSTALL of a
	// B-signed build
	list, _ := ks.List(ctx, admin, "acme", "prod")
	for _, k := range list {
		if k.ID != kB.ID {
			if _, err := ks.Retire(ctx, admin, "acme", "prod", k.ID, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	rb := &repo{t: t, b: b, dir: ra.dir}
	rb.add("loadable_extension_demo", keyB, "")
	publish(rb)
	res = newSession(t, b, opts).exec(createRepo("r", ra.dir), "LOAD loadable_extension_demo FROM r")
	mustOK(t, res[:1])
	mustFail(t, res[1], "signature")
	mustOK(t, newSession(t, b, opts).exec(
		createRepo("r", ra.dir),
		"FORCE INSTALL loadable_extension_demo FROM r",
		"LOAD loadable_extension_demo FROM r",
		"SELECT test_alias_hello()",
		pinned,
	))
}
