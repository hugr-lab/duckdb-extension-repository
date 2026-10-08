package azureblob_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/google/uuid"

	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/azureblob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob/blobtest"
)

// The well-known Azurite development account (public, documented by Microsoft).
const (
	devAccount = "devstoreaccount1"
	devKey     = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
	testBucket = "kista-test"
)

// endpoint is KISTA_TEST_AZUREBLOB: http://127.0.0.1:<port>/devstoreaccount1 (Azurite).
func endpoint(t *testing.T) string {
	t.Helper()
	v := os.Getenv("KISTA_TEST_AZUREBLOB")
	if v == "" {
		if os.Getenv("KISTA_TEST_REQUIRE_BLOB") == "1" {
			t.Fatal("KISTA_TEST_AZUREBLOB is required")
		}
		t.Skip("KISTA_TEST_AZUREBLOB not set")
	}
	return v
}

func keyFile(t *testing.T, key string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte(key+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func config(t *testing.T, ep string) azureblob.Config {
	t.Helper()
	return azureblob.Config{Endpoint: ep, Account: devAccount, Container: testBucket,
		Prefix: "t-" + uuid.NewString() + "/", AccountKeyFile: keyFile(t, devKey, 0o600)}
}

var containerOnce sync.Once

func ensureContainer(t *testing.T, ep string) {
	t.Helper()
	var err error
	containerOnce.Do(func() {
		cred, e := container.NewSharedKeyCredential(devAccount, devKey)
		if e != nil {
			err = e
			return
		}
		c, e := container.NewClientWithSharedKeyCredential(ep+"/"+testBucket, cred, nil)
		if e != nil {
			err = e
			return
		}
		if _, e := c.Create(context.Background(), nil); e != nil && !bloberror.HasCode(e, bloberror.ContainerAlreadyExists) {
			err = e
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func newStore(t *testing.T, cfg azureblob.Config) blob.Store {
	t.Helper()
	s, err := azureblob.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestContract(t *testing.T) {
	ep := endpoint(t)
	ensureContainer(t, ep)
	blobtest.Run(t, blobtest.Harness{
		New: func(t *testing.T) blob.Store { return newStore(t, config(t, ep)) },
		Steered: func(t *testing.T) blob.Store {
			for _, k := range []string{"AZURE_STORAGE_CONNECTION_STRING", "AZURE_STORAGE_ACCOUNT", "AZURE_STORAGE_KEY",
				"AZURE_STORAGE_SAS_TOKEN", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "AZURE_TENANT_ID"} {
				t.Setenv(k, "wrong")
			}
			t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
			t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
			t.Setenv("NO_PROXY", "")
			return newStore(t, config(t, ep))
		},
		Failing: func(t *testing.T) (blob.Store, []string) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.Header().Set("x-ms-request-id", blobtest.Secret)
				w.Header().Set("x-ms-error-code", "AuthorizationFailure "+blobtest.Secret)
				w.WriteHeader(http.StatusForbidden)
				if r.Method != http.MethodHead {
					_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>AuthorizationFailure</Code><Message>` +
						blobtest.Secret + " " + r.URL.String() + `</Message></Error>`))
				}
			}))
			t.Cleanup(srv.Close)
			cfg := config(t, srv.URL+"/"+devAccount)
			cfg.Container, cfg.Prefix = "kista-failing", "secret-prefix/"
			return newStore(t, cfg), []string{strings.TrimPrefix(srv.URL, "http://"), "kista-failing", "secret-prefix", devAccount}
		},
		WithTimeout: func(t *testing.T, timeout time.Duration) blob.Store {
			cfg := config(t, ep)
			cfg.Timeout = timeout
			return newStore(t, cfg)
		},
		Stalling: func(t *testing.T) (blob.Store, []string) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					t.Cleanup(func() { c.Close() })
				}
			}()
			cfg := config(t, "http://"+ln.Addr().String()+"/"+devAccount)
			cfg.Container, cfg.Prefix, cfg.Timeout = "kista-stalled", "secret-prefix/", time.Second
			return newStore(t, cfg), []string{ln.Addr().String(), "kista-stalled", "secret-prefix", devAccount}
		},
	})
}

type fakeCred struct{}

func (fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "t", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestConfigRefused(t *testing.T) {
	key := keyFile(t, devKey, 0o600)
	good := azureblob.Config{Endpoint: "https://kista.blob.core.windows.net", Account: "kista", Container: "bodies", Credential: fakeCred{}}
	for name, mut := range map[string]func(*azureblob.Config){
		"account upper":   func(c *azureblob.Config) { c.Account = "Kista" },
		"account short":   func(c *azureblob.Config) { c.Account = "ab" },
		"container":       func(c *azureblob.Config) { c.Container = "a--b" },
		"prefix":          func(c *azureblob.Config) { c.Prefix = "../" },
		"both creds":      func(c *azureblob.Config) { c.AccountKeyFile = key },
		"no creds":        func(c *azureblob.Config) { c.Credential = nil },
		"token over http": func(c *azureblob.Config) { c.Endpoint = "http://kista.blob.core.windows.net" },
		"query":           func(c *azureblob.Config) { c.Endpoint += "/?sv=2020&sig=x" },
		"key mode":        func(c *azureblob.Config) { c.Credential, c.AccountKeyFile = nil, keyFile(t, devKey, 0o644) },
		"key not base64":  func(c *azureblob.Config) { c.Credential, c.AccountKeyFile = nil, keyFile(t, "not base64!", 0o600) },
	} {
		c := good
		mut(&c)
		if _, err := azureblob.New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := azureblob.New(good); err != nil {
		t.Fatal(err)
	}
}

func TestID(t *testing.T) {
	a := newStore(t, azureblob.Config{Endpoint: "https://Kista.blob.core.windows.net:443", Account: "kista", Container: "bodies", Credential: fakeCred{}})
	b := newStore(t, azureblob.Config{Endpoint: "https://kista.blob.core.windows.net/", Account: "kista", Container: "bodies", Credential: fakeCred{}})
	if a.ID() != b.ID() || a.ID() != "azureblob:kista.blob.core.windows.net/bodies/" {
		t.Fatalf("ids %q %q", a.ID(), b.ID())
	}
}

// The anonymous check reports a public container, and a private one as private.
func TestAnonymous(t *testing.T) {
	for answer, want := range map[string][2]bool{"200": {true, true}, "404 ResourceNotFound": {false, true},
		"404 BlobNotFound": {true, true}, "409": {false, true}, "403": {false, true}, "500": {false, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" || r.Method != http.MethodHead {
				w.WriteHeader(http.StatusInternalServerError) // a signed probe must not count as private
				return
			}
			status, code, _ := strings.Cut(answer, " ")
			if code != "" {
				w.Header().Set("x-ms-error-code", code)
			}
			n, _ := strconv.Atoi(status)
			w.WriteHeader(n)
		}))
		cfg := config(t, srv.URL+"/"+devAccount)
		readable, ok := newStore(t, cfg).Anonymous(context.Background(), blob.MarkerKey)
		srv.Close()
		if readable != want[0] || ok != want[1] {
			t.Errorf("%s: readable %v ok %v", answer, readable, ok)
		}
	}
}

// Azurite's container is private: the live anonymous check says so.
func TestAnonymousLive(t *testing.T) {
	ep := endpoint(t)
	ensureContainer(t, ep)
	s := newStore(t, config(t, ep))
	if err := s.Put(context.Background(), blob.MarkerKey, strings.NewReader("{}"), 2); err != nil {
		t.Fatal(err)
	}
	if readable, ok := s.Anonymous(context.Background(), blob.MarkerKey); readable || !ok {
		t.Fatalf("readable %v ok %v", readable, ok)
	}
	if _, err := s.Stat(context.Background(), blobtest.Key([]byte("none"))); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("stat of a missing blob: %v", err)
	}
}

// A public container is detected live (Azurite), with and without the key present.
func TestAnonymousPublicLive(t *testing.T) {
	ep := endpoint(t)
	cred, _ := container.NewSharedKeyCredential(devAccount, devKey)
	c, err := container.NewClientWithSharedKeyCredential(ep+"/kista-public", cred, nil)
	if err != nil {
		t.Fatal(err)
	}
	access := container.PublicAccessTypeBlob
	if _, err := c.Create(context.Background(), &container.CreateOptions{Access: &access}); err != nil && !bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
		t.Fatal(err)
	}
	cfg := config(t, ep)
	cfg.Container = "kista-public"
	s := newStore(t, cfg)
	if readable, ok := s.Anonymous(context.Background(), blob.MarkerKey); !readable || !ok {
		t.Fatalf("public container without the key: readable %v ok %v", readable, ok)
	}
	if err := s.Put(context.Background(), blob.MarkerKey, strings.NewReader("{}"), 2); err != nil {
		t.Fatal(err)
	}
	if readable, ok := s.Anonymous(context.Background(), blob.MarkerKey); !readable || !ok {
		t.Fatalf("public container: readable %v ok %v", readable, ok)
	}
}
