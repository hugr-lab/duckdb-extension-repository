package auth

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Spec 0023: a publisher signs each request with its developer's SSH key. The request carries a
// statement (the method, the path and query, the body's SHA-256 and the signing time) and an SSH
// signature (OpenSSH's PROTOCOL.sshsig) over it, in the namespace kista-request@v1.

// Headers of a signed request.
const (
	StatementHeader = "Kista-Statement"
	SignatureHeader = "Kista-Signature"
)

// Namespace is the SSHSIG namespace of a request signature: a signature made for anything else
// (git commits, files) never verifies here.
const Namespace = "kista-request@v1"

// MaxStatement and MaxSignature bound the two headers' decoded sizes.
const (
	MaxStatement = 4096
	MaxSignature = 1024
)

// Key types a publisher key may have.
const (
	KeyEd25519   = ssh.KeyAlgoED25519  // ssh-ed25519
	KeyECDSAP256 = ssh.KeyAlgoECDSA256 // ecdsa-sha2-nistp256
)

// ErrStatement is a malformed statement or signature header: the request is refused with 400.
var ErrStatement = errors.New("auth: malformed signed request")

// Statement is a parsed request statement.
type Statement struct {
	Method, Path string
	SHA256       string // hex, lowercase: the request body's
	SignedAt     time.Time
	Raw          []byte // the statement's bytes, as signed
}

var (
	stmtMethod = regexp.MustCompile(`^[A-Z]{1,16}$`)
	stmtPath   = regexp.MustCompile(`^/[\x21-\x7e]*$`)
	stmtHash   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	stmtTime   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$`)
)

// ParseStatement parses a statement: exactly five lines, each ending in LF, in this order:
//
//	kista-request v1
//	method <METHOD>
//	path <path and query>
//	sha256 <hex>
//	signed-at <YYYY-MM-DDTHH:MM:SSZ>
func ParseStatement(b []byte) (Statement, error) {
	bad := func(what string) (Statement, error) {
		return Statement{}, fmt.Errorf("%w: the statement's %s", ErrStatement, what)
	}
	if len(b) == 0 || len(b) > MaxStatement || b[len(b)-1] != '\n' {
		return bad("form: five lines, each ending in LF, at most 4 KiB")
	}
	lines := strings.Split(string(b[:len(b)-1]), "\n")
	if len(lines) != 5 || lines[0] != "kista-request v1" {
		return bad("form: kista-request v1, then method, path, sha256 and signed-at")
	}
	field := func(line, key string, re *regexp.Regexp) (string, bool) {
		v, ok := strings.CutPrefix(line, key+" ")
		return v, ok && re.MatchString(v)
	}
	var s Statement
	var ok bool
	if s.Method, ok = field(lines[1], "method", stmtMethod); !ok {
		return bad("method")
	}
	if s.Path, ok = field(lines[2], "path", stmtPath); !ok {
		return bad("path")
	}
	if s.SHA256, ok = field(lines[3], "sha256", stmtHash); !ok {
		return bad("sha256")
	}
	at, ok := field(lines[4], "signed-at", stmtTime)
	if !ok {
		return bad("signed-at")
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return bad("signed-at")
	}
	s.SignedAt, s.Raw = t, b
	return s, nil
}

// SignedRequest is a request's statement and signature, decoded from their headers.
type SignedRequest struct {
	Statement Statement
	Signature []byte // the SSHSIG blob
	Key       ssh.PublicKey
	// Fingerprint is the signing key's, SHA256:<unpadded base64>, as ssh-keygen -l prints it.
	Fingerprint string
	sig         sshsig
}

// ParseSignedRequest decodes the two headers (standard base64 each, the signature without its
// armor) and parses the statement and the SSHSIG blob; it does not verify the signature.
func ParseSignedRequest(statement, signature string) (SignedRequest, error) {
	var r SignedRequest
	st, err := decodeHeader(statement, MaxStatement)
	if err != nil {
		return r, fmt.Errorf("%w: %s is standard base64 of the statement", ErrStatement, StatementHeader)
	}
	if r.Statement, err = ParseStatement(st); err != nil {
		return r, err
	}
	if r.Signature, err = decodeHeader(signature, MaxSignature); err != nil {
		return r, fmt.Errorf("%w: %s is standard base64 of the SSH signature, on one line without its armor", ErrStatement, SignatureHeader)
	}
	if r.sig, err = parseSSHSIG(r.Signature); err != nil {
		return r, err
	}
	r.Key, r.Fingerprint = r.sig.key, ssh.FingerprintSHA256(r.sig.key)
	return r, nil
}

func decodeHeader(v string, limit int) ([]byte, error) {
	if v == "" || len(v) > base64.StdEncoding.EncodedLen(limit) {
		return nil, ErrStatement
	}
	return base64.StdEncoding.Strict().DecodeString(v)
}

// Verify checks the signature with a registered key, given in its wire form: the signature's own
// key must be that key byte for byte, the namespace kista-request@v1, and the signature must
// verify over the statement.
func (r SignedRequest) Verify(registered []byte) error {
	if !bytes.Equal(r.sig.key.Marshal(), registered) {
		return errors.New("auth: the signature's key is not the registered one")
	}
	return r.sig.verify(r.Statement.Raw)
}

// --- SSHSIG (OpenSSH PROTOCOL.sshsig) ---

const sshsigMagic = "SSHSIG"

type sshsig struct {
	key       ssh.PublicKey
	namespace string
	hashAlg   string
	sig       ssh.Signature
}

// parseSSHSIG parses a binary SSHSIG blob: the magic, version 1, the public key, the namespace,
// the reserved string, the hash algorithm and the signature, with nothing after it. The key must
// be a type a publisher key may have, and the signature's format the key's.
func parseSSHSIG(b []byte) (sshsig, error) {
	bad := func(what string) (sshsig, error) {
		return sshsig{}, fmt.Errorf("%w: the SSH signature's %s", ErrStatement, what)
	}
	var s sshsig
	if !bytes.HasPrefix(b, []byte(sshsigMagic)) {
		return bad("magic")
	}
	b = b[len(sshsigMagic):]
	if len(b) < 4 || binary.BigEndian.Uint32(b) != 1 {
		return bad("version")
	}
	b = b[4:]
	var key, ns, reserved, alg, sig []byte
	var ok bool
	for _, f := range []*[]byte{&key, &ns, &reserved, &alg, &sig} {
		if *f, b, ok = sshString(b); !ok {
			return bad("form")
		}
	}
	if len(b) != 0 {
		return bad("form: bytes after the signature")
	}
	pk, err := ssh.ParsePublicKey(key)
	if err != nil {
		return bad("public key")
	}
	if t := pk.Type(); t != KeyEd25519 && t != KeyECDSAP256 {
		return bad("key type: ssh-ed25519 or ecdsa-sha2-nistp256")
	}
	s.key, s.namespace, s.hashAlg = pk, string(ns), string(alg)
	if s.namespace != Namespace {
		return bad("namespace: " + Namespace)
	}
	if s.hashAlg != "sha256" && s.hashAlg != "sha512" {
		return bad("hash algorithm: sha256 or sha512")
	}
	if len(reserved) != 0 {
		return bad("reserved field")
	}
	format, rest, ok := sshString(sig)
	if !ok {
		return bad("signature")
	}
	blob, rest, ok := sshString(rest)
	if !ok || len(rest) != 0 || string(format) != pk.Type() {
		return bad("signature: its format must be the key's")
	}
	s.sig = ssh.Signature{Format: string(format), Blob: blob}
	return s, nil
}

// verify checks the signature over a message: the signed data is the magic, the namespace, the
// reserved string, the hash algorithm and the message's hash.
func (s sshsig) verify(msg []byte) error {
	var h []byte
	switch s.hashAlg {
	case "sha256":
		sum := sha256.Sum256(msg)
		h = sum[:]
	case "sha512":
		sum := sha512.Sum512(msg)
		h = sum[:]
	default:
		return errors.New("auth: the signature's hash algorithm")
	}
	data := []byte(sshsigMagic)
	for _, f := range [][]byte{[]byte(s.namespace), nil, []byte(s.hashAlg), h} {
		data = appendSSHString(data, f)
	}
	return s.key.Verify(data, &s.sig)
}

func sshString(b []byte) (v, rest []byte, ok bool) {
	if len(b) < 4 {
		return nil, nil, false
	}
	n := binary.BigEndian.Uint32(b)
	if uint64(n) > uint64(len(b)-4) {
		return nil, nil, false
	}
	return b[4 : 4+n], b[4+n:], true
}

func appendSSHString(b, v []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
	return append(b, v...)
}

// --- publisher keys ---

// PublicKey is a publisher key as registered: its type, wire form, fingerprint and title.
type PublicKey struct {
	Type, Fingerprint, Title string
	Wire                     []byte
}

var keyTitle = regexp.MustCompile(`^[\x20-\x7e]{0,100}$`)

// ParsePublicKey parses an OpenSSH public key line ("ssh-ed25519 AAAA… comment"), as ssh-keygen
// writes it: an Ed25519 or ECDSA P-256 key; the comment, printable ASCII of at most 100
// characters, is its title.
func ParsePublicKey(line string) (PublicKey, error) {
	line = strings.TrimSpace(line)
	if strings.ContainsAny(line, "\r\n") || len(line) > 1024 {
		return PublicKey{}, errors.New("one OpenSSH public key line, as ssh-keygen writes it in the .pub file")
	}
	pk, comment, opts, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(opts) != 0 || len(rest) != 0 {
		return PublicKey{}, errors.New("one OpenSSH public key line, as ssh-keygen writes it in the .pub file")
	}
	if t := pk.Type(); t != KeyEd25519 && t != KeyECDSAP256 {
		return PublicKey{}, fmt.Errorf("a key of type %s: a publisher key is ssh-ed25519 or ecdsa-sha2-nistp256", t)
	}
	if !keyTitle.MatchString(comment) {
		return PublicKey{}, errors.New("the key's comment (its title) is printable ASCII of at most 100 characters")
	}
	return PublicKey{Type: pk.Type(), Fingerprint: ssh.FingerprintSHA256(pk), Title: comment, Wire: pk.Marshal()}, nil
}

// AuthorizedKey is a registered key's OpenSSH line, without a comment.
func AuthorizedKey(wire []byte) string {
	pk, err := ssh.ParsePublicKey(wire)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk)))
}

// WireOf is a stored key's wire form, from its OpenSSH line.
func WireOf(line string) ([]byte, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, err
	}
	return pk.Marshal(), nil
}

// EmptySHA256 is the SHA-256 of an empty body, a GET's.
const EmptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// NewStatement makes a request's statement: the method, the path and query as sent, the body's
// SHA-256 and the signing time (UTC, to the second).
func NewStatement(method, path string, body []byte, at time.Time) []byte {
	sum := sha256.Sum256(body)
	return []byte(fmt.Sprintf("kista-request v1\nmethod %s\npath %s\nsha256 %x\nsigned-at %s\n", method, path, sum,
		at.UTC().Truncate(time.Second).Format(time.RFC3339)))
}

// SignStatement signs a statement as ssh-keygen -Y sign -n kista-request@v1 does (hash sha512), and
// returns the SSHSIG blob: for clients written in Go, such as a pipeline whose key is in a KMS
// (ssh.NewSignerFromSigner over the KMS's crypto.Signer).
func SignStatement(s ssh.Signer, statement []byte) ([]byte, error) {
	sum := sha512.Sum512(statement)
	data := []byte(sshsigMagic)
	for _, f := range [][]byte{[]byte(Namespace), nil, []byte("sha512"), sum[:]} {
		data = appendSSHString(data, f)
	}
	sig, err := s.Sign(nil, data)
	if err != nil {
		return nil, err
	}
	out := append([]byte(sshsigMagic), 0, 0, 0, 1)
	for _, f := range [][]byte{s.PublicKey().Marshal(), []byte(Namespace), nil, []byte("sha512"), ssh.Marshal(sig)} {
		out = appendSSHString(out, f)
	}
	return out, nil
}
