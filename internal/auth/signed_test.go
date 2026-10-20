package auth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"golang.org/x/crypto/ssh"
)

// The vectors in testdata/sshsig were made by ssh-keygen (OpenSSH 10.3): keys of three types, and
// "ssh-keygen -Y sign -n kista-request@v1" over testdata/sshsig/statement (sha512, its default);
// ed25519-sha256.sig with -O hashalg=sha256, ed25519-git.sig in the namespace git.

func vector(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/sshsig/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// unarmor is what a client sends as Kista-Signature: the armored payload on one line.
func unarmor(t *testing.T, armored string) string {
	t.Helper()
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSpace(armored), "\n") {
		if !strings.HasPrefix(l, "-----") {
			b.WriteString(l)
		}
	}
	return b.String()
}

func signedVector(t *testing.T, sig string) (SignedRequest, error) {
	t.Helper()
	st := base64.StdEncoding.EncodeToString([]byte(vector(t, "statement")))
	return ParseSignedRequest(st, unarmor(t, vector(t, sig)))
}

func registered(t *testing.T, pub string) PublicKey {
	t.Helper()
	k, err := ParsePublicKey(vector(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSignedVectors(t *testing.T) {
	for _, c := range []struct{ sig, pub, typ, title string }{
		{"ed25519.sig", "ed25519.pub", KeyEd25519, "ci ed25519"},
		{"ed25519-sha256.sig", "ed25519.pub", KeyEd25519, "ci ed25519"},
		{"p256.sig", "p256.pub", KeyECDSAP256, "ci p256"},
	} {
		t.Run(c.sig, func(t *testing.T) {
			r, err := signedVector(t, c.sig)
			if err != nil {
				t.Fatal(err)
			}
			k := registered(t, c.pub)
			if k.Type != c.typ || k.Title != c.title || r.Fingerprint != k.Fingerprint || !strings.HasPrefix(k.Fingerprint, "SHA256:") {
				t.Fatalf("key %+v, request's fingerprint %s", k, r.Fingerprint)
			}
			if err := r.Verify(k.Wire); err != nil {
				t.Fatalf("verify: %v", err)
			}
			st := r.Statement
			if st.Method != "POST" || st.SHA256 != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" ||
				!st.SignedAt.Equal(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)) ||
				st.Path != "/api/v1/tenants/acme/channels/staging/extensions/acl/releases?platform=linux_amd64&version=1.4.0" {
				t.Fatalf("statement %+v", st)
			}
		})
	}
}

func TestSignedRefusals(t *testing.T) {
	ed := registered(t, "ed25519.pub")
	p256 := registered(t, "p256.pub")

	// another registered key: the signature's key must be the registered one, byte for byte
	r, err := signedVector(t, "ed25519.sig")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(p256.Wire); err == nil {
		t.Fatal("verified with another key")
	}

	// a tampered statement
	r.Statement.Raw = []byte(strings.Replace(string(r.Statement.Raw), "staging", "prod", 1))
	if err := r.Verify(ed.Wire); err == nil {
		t.Fatal("verified a tampered statement")
	}

	// the namespace git, an RSA key: refused when parsed
	for _, sig := range []string{"ed25519-git.sig", "rsa.sig"} {
		if _, err := signedVector(t, sig); !errors.Is(err, ErrStatement) {
			t.Fatalf("%s: %v", sig, err)
		}
	}
	if _, err := ParsePublicKey(vector(t, "rsa.pub")); err == nil {
		t.Fatal("an RSA key registered")
	}

	// the armored form, trailing bytes, a broken signature blob
	good := unarmor(t, vector(t, "ed25519.sig"))
	st := base64.StdEncoding.EncodeToString([]byte(vector(t, "statement")))
	blob, _ := base64.StdEncoding.DecodeString(good)
	for name, sig := range map[string]string{
		"armored":  vector(t, "ed25519.sig"),
		"trailing": base64.StdEncoding.EncodeToString(append(append([]byte{}, blob...), 0)),
		"short":    base64.StdEncoding.EncodeToString(blob[:len(blob)-10]),
		"magic":    base64.StdEncoding.EncodeToString(append([]byte("SSHSIH"), blob[6:]...)),
		"empty":    "",
	} {
		if _, err := ParseSignedRequest(st, sig); !errors.Is(err, ErrStatement) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a signature whose bytes are flipped still parses, and does not verify
	flipped := append([]byte{}, blob...)
	flipped[len(flipped)-1] ^= 1
	if r, err := ParseSignedRequest(st, base64.StdEncoding.EncodeToString(flipped)); err != nil || r.Verify(ed.Wire) == nil {
		t.Fatalf("flipped: %v", err)
	}
}

func TestParseStatement(t *testing.T) {
	good := "kista-request v1\nmethod GET\npath /api/v1/tenants/acme/whoami\nsha256 " + EmptySHA256 + "\nsigned-at 2026-10-19T12:00:00Z\n"
	if s, err := ParseStatement([]byte(good)); err != nil || s.Method != "GET" || s.Path != "/api/v1/tenants/acme/whoami" {
		t.Fatalf("%+v %v", s, err)
	}
	for name, s := range map[string]string{
		"no final LF":   strings.TrimSuffix(good, "\n"),
		"CRLF":          strings.ReplaceAll(good, "\n", "\r\n"),
		"extra line":    good + "source x\n",
		"missing line":  strings.Replace(good, "method GET\n", "", 1),
		"order":         "kista-request v1\npath /x\nmethod GET\nsha256 " + EmptySHA256 + "\nsigned-at 2026-10-19T12:00:00Z\n",
		"version":       strings.Replace(good, "v1", "v2", 1),
		"method":        strings.Replace(good, "method GET", "method get", 1),
		"path space":    strings.Replace(good, "whoami", "who ami", 1),
		"path relative": strings.Replace(good, "path /api", "path api", 1),
		"hash upper":    strings.Replace(good, "e3b0", "E3B0", 1),
		"hash short":    strings.Replace(good, EmptySHA256, EmptySHA256[:62], 1),
		"time offset":   strings.Replace(good, "12:00:00Z", "12:00:00+00:00", 1),
		"time fraction": strings.Replace(good, "12:00:00Z", "12:00:00.5Z", 1),
		"time invalid":  strings.Replace(good, "10-19", "13-40", 1),
		"double space":  strings.Replace(good, "method GET", "method  GET", 1),
		"too long":      strings.Replace(good, "whoami", "whoami?"+strings.Repeat("a", 4096), 1),
		"empty":         "",
	} {
		if _, err := ParseStatement([]byte(s)); !errors.Is(err, ErrStatement) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParsePublicKey(t *testing.T) {
	ed := vector(t, "ed25519.pub")
	k, err := ParsePublicKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	if got := AuthorizedKey(k.Wire); !strings.HasPrefix(ed, got+" ") {
		t.Fatalf("AuthorizedKey %q, from %q", got, ed)
	}
	fields := strings.Fields(ed)
	for name, line := range map[string]string{
		"two lines":     ed + ed,
		"options":       `command="x" ` + ed,
		"title":         fields[0] + " " + fields[1] + " " + strings.Repeat("t", 101),
		"title control": fields[0] + " " + fields[1] + " a\tb",
		"not base64":    fields[0] + " !!!",
		"type mismatch": "ecdsa-sha2-nistp256 " + fields[1],
		"certificate":   "ssh-ed25519-cert-v01@openssh.com " + fields[1],
		"empty":         "",
	} {
		if _, err := ParsePublicKey(line); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// no comment: no title
	if k, err := ParsePublicKey(fields[0] + " " + fields[1]); err != nil || k.Title != "" {
		t.Fatalf("%+v %v", k, err)
	}
}

// An issuer record at GitHub Actions' URL is never used (spec 0008, kept by 0023): any workflow can
// request a token from it for any audience.
func TestGitHubIssuerRefused(t *testing.T) {
	for _, u := range []string{GitHubActionsIssuer, GitHubActionsIssuer + "/", GitHubActionsIssuer + "/acme-enterprise",
		"https://TOKEN.actions.githubusercontent.com", "https://token.actions.acme.ghe.com", "https://ghes.example.com/_services/token",
		"https://ghes.example.com/_services/token/"} {
		if !RefusedIssuer(u) {
			t.Errorf("%s accepted", u)
		}
	}
	for _, u := range []string{"https://idp.example", "https://login.example/token", "https://actions.example/realms/x"} {
		if RefusedIssuer(u) {
			t.Errorf("%s refused", u)
		}
	}
	ta := store.TenantAuth{Issuers: []store.Issuer{{Name: "gh", URL: GitHubActionsIssuer}, {Name: "corp", URL: "https://idp.example"}}}
	got, canonical := ForTenant(ta, "https://kista.example", "acme")
	if len(got.Issuers) != 1 || got.Issuers[0].Name != "corp" || canonical != "https://kista.example/acme" {
		t.Fatalf("%+v %s", got.Issuers, canonical)
	}
}

// SignStatement makes what ssh-keygen makes: a signature that verifies, by Ed25519 and ECDSA keys.
func TestSignStatement(t *testing.T) {
	for _, mk := range []func() (ssh.Signer, error){
		func() (ssh.Signer, error) {
			_, k, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return nil, err
			}
			return ssh.NewSignerFromKey(k)
		},
		func() (ssh.Signer, error) {
			k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return nil, err
			}
			return ssh.NewSignerFromKey(k)
		},
	} {
		s, err := mk()
		if err != nil {
			t.Fatal(err)
		}
		st := NewStatement("GET", "/api/v1/tenants/acme/whoami", nil, time.Now())
		sig, err := SignStatement(s, st)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ParseSignedRequest(base64.StdEncoding.EncodeToString(st), base64.StdEncoding.EncodeToString(sig))
		if err != nil {
			t.Fatal(err)
		}
		if r.Statement.SHA256 != EmptySHA256 || r.Fingerprint != ssh.FingerprintSHA256(s.PublicKey()) {
			t.Fatalf("%+v", r)
		}
		if err := r.Verify(s.PublicKey().Marshal()); err != nil {
			t.Fatalf("%s: %v", s.PublicKey().Type(), err)
		}
	}
}

// ssh-keygen verifies what SignStatement makes (skipped without OpenSSH 8.1 or later).
func TestSignStatementOpenSSH(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	st := NewStatement("POST", "/api/v1/tenants/acme/channels/staging/extensions/acl/releases?platform=linux_amd64&version=1", []byte("x"), time.Now())
	sig, err := SignStatement(s, st)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	armored := "-----BEGIN SSH SIGNATURE-----\n" + base64.StdEncoding.EncodeToString(sig) + "\n-----END SSH SIGNATURE-----\n"
	allowed := "ci " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))) + "\n"
	for name, b := range map[string]string{"sig": armored, "allowed": allowed} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("ssh-keygen", "-Y", "verify", "-f", filepath.Join(dir, "allowed"), "-I", "ci", "-n", Namespace, "-s", filepath.Join(dir, "sig"))
	cmd.Stdin = bytes.NewReader(st)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen -Y verify: %v\n%s", err, out)
	}
}

// sshsigFields splits an SSHSIG blob into its five strings, and join puts them back with a version.
func sshsigFields(t *testing.T, blob []byte) [][]byte {
	t.Helper()
	b := blob[len(sshsigMagic)+4:]
	var out [][]byte
	for range 5 {
		v, rest, ok := sshString(b)
		if !ok {
			t.Fatal("not an SSHSIG blob")
		}
		out, b = append(out, v), rest
	}
	return out
}

func joinSSHSIG(version uint32, f [][]byte) string {
	b := binary.BigEndian.AppendUint32([]byte(sshsigMagic), version)
	for _, x := range f {
		b = appendSSHString(b, x)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// Each field of the SSH signature is checked: the version, the reserved string, the hash algorithm,
// and the signature's format against the key's type. A certificate's key line is refused.
func TestSSHSIGFields(t *testing.T) {
	st := base64.StdEncoding.EncodeToString([]byte(vector(t, "statement")))
	blob, _ := base64.StdEncoding.DecodeString(unarmor(t, vector(t, "ed25519.sig")))
	good := sshsigFields(t, blob)
	if _, err := ParseSignedRequest(st, joinSSHSIG(1, good)); err != nil {
		t.Fatalf("the vector put back together: %v", err)
	}
	with := func(i int, v []byte) [][]byte {
		f := append([][]byte{}, good...)
		f[i] = v
		return f
	}
	_, rest, _ := sshString(good[4])
	p256, _ := base64.StdEncoding.DecodeString(unarmor(t, vector(t, "p256.sig")))
	for name, sig := range map[string]string{
		"version 2":        joinSSHSIG(2, good),
		"reserved":         joinSSHSIG(1, with(2, []byte("x"))),
		"hash md5":         joinSSHSIG(1, with(3, []byte("md5"))),
		"hash SHA512":      joinSSHSIG(1, with(3, []byte("SHA512"))),
		"another format":   joinSSHSIG(1, with(4, append(appendSSHString(nil, []byte("ecdsa-sha2-nistp256")), rest...))),
		"another key":      joinSSHSIG(1, with(0, sshsigFields(t, p256)[0])),
		"rsa-sha2 format":  joinSSHSIG(1, with(4, append(appendSSHString(nil, []byte("rsa-sha2-512")), rest...))),
		"namespace prefix": joinSSHSIG(1, with(1, []byte("kista-request@v1x"))),
	} {
		r, err := ParseSignedRequest(st, sig)
		if err == nil {
			// parsed: it must not verify with the registered key either
			err = r.Verify(registered(t, "ed25519.pub").Wire)
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
