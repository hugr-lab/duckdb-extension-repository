package config

import (
	"strings"
	"testing"
)

const s3Domain = `
blob:
  domains:
    - name: default
      kind: s3
      s3: { endpoint: "https://s3.eu-north-1.amazonaws.com", bucket: kista-bodies, prefix: prod/, region: eu-north-1,
            lookup: dns, sse: kms, kms_key_id: alias/kista, access_key_file: /run/secrets/a, secret_key_file: /run/secrets/s }
    - name: local
      kind: fs
      fs: { root: /data/blobs }
  spool_dir: /var/spool/kista
  max_body: 512MiB
  max_ingests: 2
`

func TestBlob(t *testing.T) {
	cfg, err := Load(write(t, base+s3Domain), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.BlobDomains()) != 2 || !cfg.HasBlobDomain("local") || cfg.HasBlobDomain("cn") || cfg.SpoolDir() != "/var/spool/kista" {
		t.Fatalf("%+v", cfg.Blob)
	}
	if mb, mi := cfg.BlobLimits(); mb != 512<<20 || mi != 2 {
		t.Fatalf("limits %d %d", mb, mi)
	}

	// defaults: one fs domain; in dev with SQLite, beside the database
	cfg, err = Load(write(t, base), nil)
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.BlobDomains()
	if len(d) != 1 || d[0].Name != "default" || d[0].FS.Root != DefaultBlobRoot || cfg.SpoolDir() != DefaultSpoolDir {
		t.Fatalf("defaults %+v %s", d, cfg.SpoolDir())
	}
	if mb, mi := cfg.BlobLimits(); mb != 1<<30 || mi != 4 {
		t.Fatalf("default limits %d %d", mb, mi)
	}
	cfg, err = Load(write(t, "profile: dev\nstore: { kind: sqlite, sqlite: { path: /home/me/kista/kista.db } }\nrotation: { min_trusted: 1h, min_demoted: 1h }\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BlobDomains()[0].FS.Root != "/home/me/kista/blobs" || cfg.SpoolDir() != "/home/me/kista/spool" {
		t.Fatalf("dev defaults %s %s", cfg.BlobDomains()[0].FS.Root, cfg.SpoolDir())
	}
}

func TestBlobRefused(t *testing.T) {
	dom := func(body string) string { return base + "blob:\n  domains:\n" + body }
	s3 := func(fields string) string {
		return dom("    - { name: default, kind: s3, s3: { " + fields + " } }\n")
	}
	ok := `endpoint: "https://e.example", bucket: kista, region: auto, lookup: path, sse: none, access_key_file: /a, secret_key_file: /s`
	cases := map[string]struct{ file, want string }{
		"from env":           {base, ""},
		"bad name":           {dom("    - { name: Default, kind: fs, fs: { root: /a } }\n"), "name must match"},
		"twice":              {dom("    - { name: a, kind: fs, fs: { root: /a } }\n    - { name: a, kind: fs, fs: { root: /b } }\n"), "defined twice"},
		"relative root":      {dom("    - { name: a, kind: fs, fs: { root: a } }\n"), "clean absolute"},
		"two blocks":         {dom("    - { name: a, kind: fs, fs: { root: /a }, s3: {} }\n"), "exactly one block"},
		"azureblob":          {dom("    - { name: a, kind: azureblob, azureblob: {} }\n"), "phase 2"},
		"fs overlap":         {dom("    - { name: a, kind: fs, fs: { root: /a } }\n    - { name: b, kind: fs, fs: { root: /a/b } }\n"), "share a store"},
		"s3 overlap":         {dom("    - { name: a, kind: s3, s3: { " + ok + " } }\n    - { name: b, kind: s3, s3: { " + ok + ", prefix: x/ } }\n"), "share a store"},
		"http":               {s3(strings.Replace(ok, "https://e.example", "http://e.example", 1)), "plain http only on loopback"},
		"http loopback prod": {s3(strings.Replace(ok, "https://e.example", "http://127.0.0.1:9000", 1)), "plain http only on loopback"},
		"endpoint path":      {s3(strings.Replace(ok, "https://e.example", "https://e.example/x", 1)), "scheme://host"},
		"endpoint creds":     {s3(strings.Replace(ok, "https://e.example", "https://a:b@e.example", 1)), "scheme://host"},
		"no region":          {s3(strings.Replace(ok, "region: auto", "region: ''", 1)), "region is required"},
		"no sse":             {s3(strings.Replace(ok, "sse: none, ", "", 1)), "sse is required"},
		"kms without key":    {s3(strings.Replace(ok, "sse: none", "sse: kms", 1)), "kms_key_id is required"},
		"key without kms":    {s3(ok + ", kms_key_id: k"), "only for sse kms"},
		"bad prefix":         {s3(ok + ", prefix: ../"), "prefix"},
		"prefix no slash":    {s3(ok + ", prefix: x"), "prefix"},
		"bad bucket":         {s3(strings.Replace(ok, "bucket: kista", "bucket: Kista", 1)), "bucket"},
		"no lookup":          {s3(strings.Replace(ok, "lookup: path, ", "", 1)), "lookup"},
		"relative key file":  {s3(strings.Replace(ok, "/a", "a", 1)), "absolute paths"},
		"max_body":           {base + "blob: { max_body: 5GiB }\n", "max_body"},
		"max_body garbage":   {base + "blob: { max_body: lots }\n", "not a size"},
		"relative spool":     {base + "blob: { spool_dir: spool }\n", "spool_dir"},
		"fs timeout":         {dom("    - { name: default, kind: fs, fs: { root: /a }, timeout: 10s }\n"), "remote stores only"},
		"s3 timeout":         {dom("    - { name: default, kind: s3, timeout: 1h, s3: { " + ok + " } }\n"), "1s..5m"},
		"no default":         {dom("    - { name: eu, kind: fs, fs: { root: /a } }\n"), "must include"},
		"dotted dns https":   {s3(strings.Replace(strings.Replace(ok, "bucket: kista", "bucket: a.b.c", 1), "lookup: path", "lookup: dns", 1)), "dots"},
		"ip bucket":          {s3(strings.Replace(ok, "bucket: kista", "bucket: 10.0.0.1", 1)), "bucket"},
		"relative ca":        {s3(ok + ", ca_file: ca.pem"), "ca_file"},
	}
	for name, c := range cases {
		if name == "from env" {
			if _, err := Load(write(t, base), []string{"KISTA_BLOB__SPOOL_DIR=/tmp"}); err == nil || !strings.Contains(err.Error(), "can only be set in the config file") {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		_, err := Load(write(t, c.file), nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	if _, err := Load(write(t, s3(ok)), nil); err != nil {
		t.Fatalf("a valid s3 domain: %v", err)
	}
	dev := "profile: dev\nrotation: { min_trusted: 1h, min_demoted: 1h }\n" + s3(strings.Replace(ok, "https://e.example", "http://127.0.0.1:9000", 1))
	if _, err := Load(write(t, dev), nil); err != nil {
		t.Fatalf("http on loopback in dev: %v", err)
	}
}
