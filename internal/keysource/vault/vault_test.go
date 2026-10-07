package vault_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource"
	"github.com/hugr-lab/duckdb-extension-repository/internal/keysource/vault"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi"
	"github.com/hugr-lab/duckdb-extension-repository/internal/vaultapi/vaulttest"
)

var ctx = context.Background()

func registry(t *testing.T, api vault.Requester, mount string, opts keysource.Options) *keysource.Registry {
	t.Helper()
	reg := keysource.NewRegistry(signer.Resolver{})
	if opts.Allow == nil {
		opts.Allow = []string{"ext-"}
	}
	if err := reg.Add("bao", vault.New(api, mount), opts); err != nil {
		t.Fatal(err)
	}
	return reg
}

func client(t *testing.T, cfg vaultapi.Config) *vaultapi.Client {
	t.Helper()
	cfg.AllowHTTP = true
	c, err := vaultapi.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func digest() extfile.BodyHash { return extfile.BodyHash(sha256.Sum256([]byte("an extension body"))) }

// --- real Vault and OpenBao ---

func TestRealServers(t *testing.T) {
	servers := vaulttest.Servers(t)
	if len(servers) == 0 {
		t.Skip("KISTA_TEST_VAULT / KISTA_TEST_OPENBAO are not set")
	}
	for _, srv := range servers {
		t.Run(srv.Name, func(t *testing.T) {
			tr := srv.NewTransit(t)
			api := client(t, vaultapi.Config{Address: srv.Address, AuthKind: vaultapi.AuthTokenFile, TokenFile: tr.TokenFile})
			reg := registry(t, api, tr.Mount, keysource.Options{})
			ref := "bao:" + tr.Key + ":v1"
			sg, err := reg.Open(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if sg.ID() != ref {
				t.Fatalf("ID %q", sg.ID())
			}
			h := digest()
			sig, err := sg.Sign(ctx, h)
			if err != nil {
				t.Fatal(err)
			}
			// exactly what DuckDB verifies: PKCS#1 v1.5 with the SHA-256 DigestInfo over the digest
			if _, ok := extfile.Verify(h, sig, []*rsa.PublicKey{sg.Public()}); !ok {
				t.Fatal("the signature does not verify as DuckDB verifies")
			}
			sig2, _ := sg.Sign(ctx, h)
			if string(sig) != string(sig2) {
				t.Fatal("PKCS#1 v1.5 signatures are deterministic; got two different ones")
			}

			t.Run("jwt login", func(t *testing.T) {
				mount, role, file := tr.JWTLogin(t)
				api := client(t, vaultapi.Config{Address: srv.Address, AuthKind: vaultapi.AuthJWT, AuthMount: mount, Role: role, TokenFile: file})
				sg, err := registry(t, api, tr.Mount, keysource.Options{}).Open(ctx, ref)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := sg.Sign(ctx, h); err != nil {
					t.Fatal(err)
				}
			})

			t.Run("a token that may encrypt is refused", func(t *testing.T) {
				broad := "broad-" + tr.Mount
				tr.Root(t, http.MethodPut, "sys/policies/acl/"+broad, map[string]any{"policy": "path \"" + tr.Mount + "/*\" { capabilities = [\"read\", \"update\"] }\n" +
					"path \"sys/capabilities-self\" { capabilities = [\"update\"] }\n"})
				api := client(t, vaultapi.Config{Address: srv.Address, AuthKind: vaultapi.AuthTokenFile, TokenFile: tr.WriteToken(t, broad)})
				if _, err := registry(t, api, tr.Mount, keysource.Options{}).Open(ctx, ref); !errors.Is(err, keysource.ErrKey) {
					t.Fatalf("got %v", err)
				}
			})

			t.Run("versions", func(t *testing.T) {
				if _, err := reg.Open(ctx, "bao:"+tr.Key+":v2"); !errors.Is(err, keysource.ErrKey) {
					t.Fatalf("missing version: %v", err)
				}
				for _, bad := range []string{tr.Key + ":v0", tr.Key + ":v01", tr.Key, tr.Key + ":latest", "other-key:v1", "../x:v1"} {
					if _, err := reg.Open(ctx, "bao:"+bad); !errors.Is(err, keysource.ErrReference) {
						t.Errorf("%s: %v", bad, err)
					}
				}
			})

			t.Run("a policy widened after open stops signing", func(t *testing.T) {
				tr := srv.NewTransit(t) // its own mount and policy: this case rewrites the policy
				ref := "bao:" + tr.Key + ":v1"
				api := client(t, vaultapi.Config{Address: srv.Address, AuthKind: vaultapi.AuthTokenFile, TokenFile: tr.TokenFile})
				sg, err := registry(t, api, tr.Mount, keysource.Options{Recheck: time.Nanosecond}).Open(ctx, ref)
				if err != nil {
					t.Fatal(err)
				}
				// the same policy name, now also allowing decrypt
				tr.Root(t, http.MethodPut, "sys/policies/acl/"+tr.Policy, map[string]any{"policy": fmt.Sprintf(
					"path %q { capabilities = [\"read\"] }\npath %q { capabilities = [\"update\"] }\npath %q { capabilities = [\"update\"] }\n"+
						"path \"sys/capabilities-self\" { capabilities = [\"update\"] }\n",
					tr.Mount+"/keys/"+tr.Key, tr.Mount+"/sign/"+tr.Key+"/sha2-256", tr.Mount+"/decrypt/"+tr.Key)})
				if _, err := sg.Sign(ctx, h); !errors.Is(err, keysource.ErrKey) {
					t.Fatalf("signed with a token that may decrypt: %v", err)
				}
			})

			t.Run("a property flipped later stops signing", func(t *testing.T) {
				api := client(t, vaultapi.Config{Address: srv.Address, AuthKind: vaultapi.AuthTokenFile, TokenFile: tr.TokenFile})
				sg, err := registry(t, api, tr.Mount, keysource.Options{Recheck: time.Nanosecond}).Open(ctx, ref)
				if err != nil {
					t.Fatal(err)
				}
				tr.SetConfig(t, map[string]any{"exportable": true})
				if _, err := sg.Sign(ctx, h); !errors.Is(err, keysource.ErrKey) {
					t.Fatalf("signed with an exportable key: %v", err)
				}
				if _, err := reg.Open(ctx, ref); !errors.Is(err, keysource.ErrKey) {
					t.Fatalf("opened an exportable key: %v", err)
				}
			})
		})
	}
}

// --- a fake Vault, for answers a real server does not give ---

type fake struct {
	key       *rsa.PrivateKey
	info      map[string]any
	caps      map[string]any // per path override; default: read on the key, deny elsewhere
	capsRaw   string         // a raw capabilities-self answer, if set
	hang      atomic.Bool
	sigVer    int // key_version reported by sign; 0 = the requested one
	corrupt   bool
	fail5xx   atomic.Int32
	errorBody string
}

func newFake(t *testing.T) (*fake, *httptest.Server) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	f := &fake{key: k, caps: map[string]any{}, info: map[string]any{
		"type": "rsa-2048", "supports_signing": true, "exportable": false, "allow_plaintext_backup": false,
		"imported_key": false, "latest_version": 1, "min_encryption_version": 0,
		"keys": map[string]any{"1": map[string]any{"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.fail5xx.Load() > 0 {
			f.fail5xx.Add(-1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if f.hang.Load() {
			<-r.Context().Done()
			return
		}
		if f.errorBody != "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(f.errorBody))
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/kt/keys/"):
			json.NewEncoder(w).Encode(map[string]any{"data": f.info})
		case r.URL.Path == "/v1/sys/capabilities-self":
			if f.capsRaw != "" {
				w.Write([]byte(f.capsRaw))
				return
			}
			var req struct{ Paths []string }
			json.NewDecoder(r.Body).Decode(&req)
			data := map[string]any{}
			for _, p := range req.Paths {
				switch c, ok := f.caps[p]; {
				case ok && c == nil:
					// leave the path out of the answer
				case ok:
					data[p] = c
				case p == "kt/keys/ext-a":
					data[p] = []string{"read"}
				default:
					data[p] = []string{"deny"}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"data": data})
		case strings.HasPrefix(r.URL.Path, "/v1/kt/sign/"):
			var req struct {
				Input      string `json:"input"`
				KeyVersion int    `json:"key_version"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			d, _ := base64.StdEncoding.DecodeString(req.Input)
			sig, _ := rsa.SignPKCS1v15(nil, f.key, crypto.SHA256, d)
			if f.corrupt {
				sig[0] ^= 1
			}
			v := req.KeyVersion
			if f.sigVer != 0 {
				v = f.sigVer
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"signature": "vault:v" + itoa(v) + ":" + base64.StdEncoding.EncodeToString(sig), "key_version": v}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func tokenFile(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "token")
	os.WriteFile(p, []byte("tok"), 0o600)
	return p
}

func fakeRegistry(t *testing.T, srv *httptest.Server, opts keysource.Options) *keysource.Registry {
	return registry(t, client(t, vaultapi.Config{Address: srv.URL, AuthKind: vaultapi.AuthTokenFile, TokenFile: tokenFile(t)}), "kt", opts)
}

func TestFakeKnownAnswer(t *testing.T) {
	f, srv := newFake(t)
	sg, err := fakeRegistry(t, srv, keysource.Options{}).Open(ctx, "bao:ext-a:v1")
	if err != nil {
		t.Fatal(err)
	}
	h := digest()
	sig, err := sg.Sign(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := rsa.SignPKCS1v15(nil, f.key, crypto.SHA256, h[:])
	if string(sig) != string(want) {
		t.Fatal("not the bytes rsa.SignPKCS1v15 produces")
	}
}

func TestFakeRefusals(t *testing.T) {
	cases := map[string]func(f *fake){
		"exportable":               func(f *fake) { f.info["exportable"] = true },
		"plaintext backup":         func(f *fake) { f.info["allow_plaintext_backup"] = true },
		"imported":                 func(f *fake) { f.info["imported_key"] = true },
		"soft deleted":             func(f *fake) { f.info["soft_deleted"] = true },
		"wrong type":               func(f *fake) { f.info["type"] = "ecdsa-p256" },
		"cannot sign":              func(f *fake) { f.info["supports_signing"] = false },
		"below min encryption":     func(f *fake) { f.info["min_encryption_version"] = 2; f.info["latest_version"] = 2 },
		"token may encrypt":        func(f *fake) { f.caps["kt/encrypt/ext-a"] = []string{"update"} },
		"imported_key unreported":  func(f *fake) { delete(f.info, "imported_key") },
		"token may update the key": func(f *fake) { f.caps["kt/keys/ext-a"] = []string{"read", "update"} },
		"path missing from answer": func(f *fake) { f.caps["kt/backup/ext-a"] = nil },
		"caps not a list":          func(f *fake) { f.caps["kt/backup/ext-a"] = "deny" },
		"empty data":               func(f *fake) { f.capsRaw = `{"data":{}}` },
		"e = 3":                    func(f *fake) { pub := f.key.PublicKey; pub.E = 3; setPub(f, &pub) },
		"3072-bit key":             func(f *fake) { k, _ := rsa.GenerateKey(rand.Reader, 3072); setPub(f, &k.PublicKey) },
		"public key not readable":  func(f *fake) { f.info["keys"] = map[string]any{"1": map[string]any{"public_key": "nope"}} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f, srv := newFake(t)
			mutate(f)
			if _, err := fakeRegistry(t, srv, keysource.Options{}).Open(ctx, "bao:ext-a:v1"); !errors.Is(err, keysource.ErrKey) {
				t.Fatalf("got %v, want ErrKey", err)
			}
		})
	}
}

func setPub(f *fake, pub *rsa.PublicKey) {
	der, _ := x509.MarshalPKIXPublicKey(pub)
	f.info["keys"] = map[string]any{"1": map[string]any{"public_key": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}
}

func TestFakeSignChecks(t *testing.T) {
	f, srv := newFake(t)
	sg, err := fakeRegistry(t, srv, keysource.Options{}).Open(ctx, "bao:ext-a:v1")
	if err != nil {
		t.Fatal(err)
	}
	f.sigVer = 2 // the backend says another version signed
	if _, err := sg.Sign(ctx, digest()); !errors.Is(err, keysource.ErrSign) {
		t.Fatalf("other version: %v", err)
	}
	f.sigVer, f.corrupt = 0, true
	if _, err := sg.Sign(ctx, digest()); !errors.Is(err, keysource.ErrSign) {
		t.Fatalf("bad signature: %v", err)
	}
	f.corrupt = false
	f.fail5xx.Store(2) // two transient failures, then success: within 3 attempts
	if _, err := sg.Sign(ctx, digest()); err != nil {
		t.Fatalf("transient failures: %v", err)
	}
	f.fail5xx.Store(3)
	if _, err := sg.Sign(ctx, digest()); err == nil {
		t.Fatal("more than 3 attempts")
	}
	f.errorBody = `{"errors":["permission denied"],"secret":"hunter2-leak"}`
	if _, err := sg.Sign(ctx, digest()); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error leaked the body: %v", err)
	}
}

func TestAllowList(t *testing.T) {
	_, srv := newFake(t)
	reg := fakeRegistry(t, srv, keysource.Options{Allow: []string{"ext-"}})
	for _, ref := range []string{"bao:ext:v1", "bao:extra:v1", "nope:ext-a:v1", "bao", "bao:", "http://x:v1"} {
		if _, _, err := reg.Parse(ref); !errors.Is(err, keysource.ErrReference) {
			t.Errorf("%s: %v", ref, err)
		}
	}
	if _, k, err := reg.Parse("bao:ext-a:v12"); err != nil || k.Version != "12" || k.Ref != "bao:ext-a:v12" {
		t.Fatalf("%+v %v", k, err)
	}
}

// Every capability that would let kista use the key for anything but signing is refused, one path
// at a time.
func TestFakeEachDeniedPath(t *testing.T) {
	for _, p := range []string{
		"kt/encrypt/ext-a", "kt/decrypt/ext-a", "kt/rewrap/ext-a", "kt/datakey/plaintext/ext-a", "kt/datakey/wrapped/ext-a",
		"kt/backup/ext-a", "kt/restore/ext-a", "kt/keys/ext-a/config", "kt/keys/ext-a/rotate", "kt/keys/ext-a/trim",
		"kt/export/encryption-key/ext-a", "kt/export/signing-key/ext-a/1", "kt/export/hmac-key/ext-a/latest",
	} {
		f, srv := newFake(t)
		f.caps[p] = []string{"update"}
		if _, err := fakeRegistry(t, srv, keysource.Options{}).Open(ctx, "bao:ext-a:v1"); !errors.Is(err, keysource.ErrKey) {
			t.Errorf("%s granted: %v", p, err)
		}
	}
}

// Properties that change after open stop signing at the next re-check.
func TestFakeChangesAfterOpen(t *testing.T) {
	for name, mutate := range map[string]func(f *fake){
		"policy widened":     func(f *fake) { f.caps["kt/decrypt/ext-a"] = []string{"update"} },
		"public key changed": func(f *fake) { k, _ := rsa.GenerateKey(rand.Reader, 2048); setPub(f, &k.PublicKey) },
		"made exportable":    func(f *fake) { f.info["exportable"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			f, srv := newFake(t)
			sg, err := fakeRegistry(t, srv, keysource.Options{Recheck: time.Nanosecond}).Open(ctx, "bao:ext-a:v1")
			if err != nil {
				t.Fatal(err)
			}
			mutate(f)
			if _, err := sg.Sign(ctx, digest()); !errors.Is(err, keysource.ErrKey) {
				t.Fatalf("signed after the change: %v", err)
			}
		})
	}
}

func TestFakeTimeoutAndLeaks(t *testing.T) {
	f, srv := newFake(t)
	f.hang.Store(true)
	start := time.Now()
	if _, err := fakeRegistry(t, srv, keysource.Options{Timeout: 200 * time.Millisecond}).Open(ctx, "bao:ext-a:v1"); err == nil {
		t.Fatal("opened a hanging server")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("open took %s", time.Since(start))
	}
	f.hang.Store(false)
	f.errorBody = `{"errors":["permission denied for entity hunter2-principal"]}`
	if _, err := fakeRegistry(t, srv, keysource.Options{}).Open(ctx, "bao:ext-a:v1"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("Vault's message leaked: %v", err)
	}
}

// The reference grammar, without a server.
func TestGrammar(t *testing.T) {
	_, srv := newFake(t)
	reg := fakeRegistry(t, srv, keysource.Options{Allow: []string{"ext-"}})
	for _, ref := range []string{"bao:ext-a:v0", "bao:ext-a:v01", "bao:ext-a", "bao:ext-a:latest", "bao:ext-a:v-1",
		"bao:ext-a/../x:v1", "bao:ext%2Fa:v1", "bao:ext-a:v1:v2"} {
		if _, _, err := reg.Parse(ref); !errors.Is(err, keysource.ErrReference) {
			t.Errorf("%s: %v", ref, err)
		}
	}
}
