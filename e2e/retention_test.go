package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0042: DuckDB at the pin finds a retired version missing (the same answer as a version that
// never existed) while the flat path still installs the current version; the versioned install
// works again after a restore; after a purge (the pass with a grace of 0) it fails for good.
func TestRetention(t *testing.T) {
	b := needBuild(t)
	k := startKista(t, b)
	ctx := context.Background()
	const demo = "loadable_extension_demo"
	old := k.release(demo, release.AddOptions{})
	f, err := os.Open(withVersion(t, b.extensions[demo], "e2e-2"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := k.rel.Add(ctx, serveAdmin, "acme", "prod", f, release.AddOptions{Name: demo}); err != nil {
		t.Fatal(err)
	}
	f.Close()
	pem := k.pem(t, k.activeKey(t))
	version := "SELECT extension_version FROM duckdb_extensions() WHERE extension_name = '" + demo + "'"
	install := func(stmt string) []result {
		return newSession(t, b, nil).exec(createRepo("r", k.httpURL(), pem), stmt, "LOAD "+demo+" FROM r", version)
	}
	versioned := "INSTALL " + demo + " FROM r VERSION " + sqlString(old.PackageVersion)
	mustOK(t, install(versioned))
	get := func(v string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(k.httpURL() + "/" + demo + "/" + v + "/" + b.versionDir + "/" + b.platform + "/" + demo + ".duckdb_extension.gz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	// keep 1 for an hour: the pass retires the older version
	keep, hour := 1, time.Hour
	if _, err := k.rel.SetChannelRetention(ctx, serveAdmin, "acme", "prod", store.RetentionRule{Keep: &keep, Grace: &hour}, ""); err != nil {
		t.Fatal(err)
	}
	ch, err := k.st.GetChannel(ctx, "acme", "prod")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := k.rel.RetainChannel(ctx, ch.ID, nil); err != nil || res.Retired != 1 {
		t.Fatalf("the pass: %+v %v", res, err)
	}
	res := newSession(t, b, nil).exec(createRepo("r", k.httpURL(), pem), versioned)
	mustFail(t, res[1], "failed to download") // over http DuckDB shows the status: the missing answer
	gotStatus, gotBody := get(old.PackageVersion)
	missStatus, missBody := get("never-released")
	if gotStatus == 200 || gotStatus != missStatus || !bytes.Equal(gotBody, missBody) {
		t.Fatalf("a retired version answers %d %q, a missing one %d %q", gotStatus, gotBody, missStatus, missBody)
	}
	res = install("INSTALL " + demo + " FROM r")
	mustOK(t, res)
	if got := res[len(res)-1].Rows[0][0]; got != "e2e-2" {
		t.Fatalf("the flat path serves %v", got)
	}

	// restored: the versioned install works again
	if _, err := k.rel.Apply(ctx, serveAdmin, "acme", "prod", "", old.ID, release.Restore, 0); err != nil {
		t.Fatal(err)
	}
	res = install(versioned)
	mustOK(t, res)
	if got := res[len(res)-1].Rows[0][0]; got != old.PackageVersion {
		t.Fatalf("the restored version installs %v", got)
	}

	// a grace of 0: retired, then purged by the next pass, and never installed again
	var zero time.Duration
	if _, err := k.rel.SetChannelRetention(ctx, serveAdmin, "acme", "prod", store.RetentionRule{Keep: &keep, Grace: &zero}, ""); err != nil {
		t.Fatal(err)
	}
	if res, err := k.rel.RetainChannel(ctx, ch.ID, nil); err != nil || res.Retired != 1 {
		t.Fatalf("retired again: %+v %v", res, err)
	}
	if res, err := k.rel.RetainChannel(ctx, ch.ID, nil); err != nil || res.Purged != 1 {
		t.Fatalf("the purge: %+v %v", res, err)
	}
	res = newSession(t, b, nil).exec(createRepo("r", k.httpURL(), pem), versioned)
	mustFail(t, res[1], "failed to download") // over http DuckDB shows the status: the missing answer
	if _, err := k.rel.Apply(ctx, serveAdmin, "acme", "prod", "", old.ID, release.Restore, 0); err == nil {
		t.Fatal("a purged release restored")
	}
}
