package aws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The prefix table agrees with the pinned SDK's partitions.json: every region it lists is in its
// partition, so a new partition fails here, not in a deployment.
func TestPartitionsMatchSDK(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/aws/aws-sdk-go-v2").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "internal", "endpoints", "awsrulesfn", "partitions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Partitions []struct {
			ID      string                     `json:"id"`
			Regions map[string]json.RawMessage `json:"regions"`
		} `json:"partitions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, p := range doc.Partitions {
		for r := range p.Regions {
			if !ValidRegion(r) {
				continue // the pseudo-regions (aws-global, aws-cn-global, …)
			}
			seen++
			if got := Partition(r); got != p.ID {
				t.Errorf("region %s: partition %s, the SDK says %s", r, got, p.ID)
			}
		}
	}
	if seen < 40 {
		t.Fatalf("only %d regions read from partitions.json", seen)
	}
}

func TestShapes(t *testing.T) {
	for _, r := range []string{"eu-central-1", "us-gov-west-1", "cn-north-1", "eusc-de-east-1", "us-isob-east-1", "ap-southeast-10"} {
		if !ValidRegion(r) {
			t.Errorf("region %s refused", r)
		}
	}
	for _, r := range []string{"", "EU-central-1", "eu-central", "eu-central-1.evil", "eu-central-1/x", "aws-global"} {
		if ValidRegion(r) {
			t.Errorf("region %q accepted", r)
		}
	}
	if !ValidRoleARN("arn:aws:iam::123456789012:role/kista", "eu-central-1") || ValidRoleARN("arn:aws:iam::123456789012:role/kista", "cn-north-1") ||
		ValidRoleARN("arn:aws:iam::123456789012:user/kista", "eu-central-1") || !ValidRoleARN("arn:aws-cn:iam::123456789012:role/a/b", "cn-north-1") {
		t.Error("role ARN rules")
	}
	for host, ok := range map[string]bool{
		"s3.eu-central-1.amazonaws.com": true, "S3.EU-CENTRAL-1.AMAZONAWS.COM": true, "s3-fips.dualstack.us-gov-west-1.amazonaws.com": true,
		"s3.dualstack.eu-central-1.amazonaws.com": true, "s3.cn-north-1.amazonaws.com.cn": true,
		"s3-fips.cn-north-1.amazonaws.com.cn": false, "s3.amazonaws.com": false, "s3.us-east-1.amazonaws.com": false,
		"bucket.s3.eu-central-1.amazonaws.com": false, "s3.eu-central-1.amazonaws.com.evil.example": false, "127.0.0.1": false,
		"s3.eu-central-1.amazonaws.com.": false,
	} {
		if S3Host(host, "eu-central-1") != ok && !strings.Contains(host, "us-gov") && !strings.Contains(host, "cn-north") {
			t.Errorf("S3Host(%q) = %v", host, !ok)
		}
	}
	if !S3Host("s3-fips.dualstack.us-gov-west-1.amazonaws.com", "us-gov-west-1") || !S3Host("s3.cn-north-1.amazonaws.com.cn", "cn-north-1") ||
		S3Host("s3-fips.cn-north-1.amazonaws.com.cn", "cn-north-1") || S3Host("s3.us-iso-east-1.c2s.ic.gov", "us-iso-east-1") {
		t.Error("partitions' S3 hosts")
	}
}

// fakeEnv is an environment and files for Credentials.
func fakeEnv(vars map[string]string, files map[string]string) Env {
	read := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("no file")
	}
	return Env{
		Getenv: func(k string) string { return vars[k] },
		Environ: func() []string {
			var out []string
			for k, v := range vars {
				out = append(out, k+"="+v)
			}
			return out
		},
		ReadFile:  read,
		ReadToken: read,
	}
}

// Each kind takes the platform's variables only in their accepted form; steering variables are
// refused outside dev; static only in dev.
func TestCredentialsRules(t *testing.T) {
	const region = "eu-central-1"
	role := "arn:aws:iam::123456789012:role/kista"
	cases := []struct {
		name string
		id   Identity
		vars map[string]string
		dev  bool
		want string // "" is accepted
	}{
		{"pod identity", Identity{Kind: "pod_identity"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials",
			"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "/t"}, false, ""},
		{"pod identity v6", Identity{Kind: "pod_identity"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://[fd00:ec2::23]/v1/credentials",
			"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "/t"}, false, ""},
		{"pod identity elsewhere", Identity{Kind: "pod_identity"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://evil.example/v1/credentials",
			"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "/t"}, false, "Pod Identity"},
		{"pod identity no token", Identity{Kind: "pod_identity"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials"}, false, "Pod Identity"},
		{"ecs", Identity{Kind: "ecs"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v2/credentials/abc-123"}, false, ""},
		{"ecs at", Identity{Kind: "ecs"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "@evil.example/x"}, false, "169.254.170.2"},
		{"ecs at path", Identity{Kind: "ecs"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/x@evil.example/x"}, false, "169.254.170.2"},
		{"ecs double slash", Identity{Kind: "ecs"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "//evil.example/x"}, false, "169.254.170.2"},
		{"ecs escaped", Identity{Kind: "ecs"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/%2F%2Fevil.example"}, false, "169.254.170.2"},
		{"ecs port", Identity{Kind: "ecs"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": ":8080/x"}, false, "169.254.170.2"},
		{"ecs dots", Identity{Kind: "ecs"}, map[string]string{"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/a/../b"}, false, "169.254.170.2"},
		{"irsa", Identity{Kind: "irsa", PlatformRole: role}, map[string]string{"AWS_ROLE_ARN": role, "AWS_WEB_IDENTITY_TOKEN_FILE": "/t"}, false, ""},
		{"irsa other role", Identity{Kind: "irsa", PlatformRole: role}, map[string]string{"AWS_ROLE_ARN": role + "2", "AWS_WEB_IDENTITY_TOKEN_FILE": "/t"}, false, "equal"},
		{"irsa no config role", Identity{Kind: "irsa"}, map[string]string{"AWS_ROLE_ARN": role, "AWS_WEB_IDENTITY_TOKEN_FILE": "/t"}, false, "identity.role_arn"},
		{"imds", Identity{Kind: "imds"}, nil, false, ""},
		{"assume", Identity{Kind: "imds", AssumeRole: role}, nil, false, ""},
		{"assume other partition", Identity{Kind: "imds", AssumeRole: "arn:aws-cn:iam::123456789012:role/x"}, nil, false, "partition"},
		{"static outside dev", Identity{Kind: "static", AccessKeyFile: "/a", SecretKeyFile: "/s"}, nil, false, "dev"},
		{"static in dev", Identity{Kind: "static", AccessKeyFile: "/a", SecretKeyFile: "/s"}, nil, true, ""},
		{"unknown", Identity{Kind: "managed"}, nil, false, "kind"},
		{"steering", Identity{Kind: "imds"}, map[string]string{"AWS_PROFILE": "prod"}, false, "AWS_PROFILE"},
		{"steering endpoint", Identity{Kind: "imds"}, map[string]string{"AWS_ENDPOINT_URL_KMS": "http://x"}, false, "AWS_ENDPOINT_URL_KMS"},
		{"steering fips", Identity{Kind: "imds"}, map[string]string{"AWS_USE_FIPS_ENDPOINT": "true"}, false, "AWS_USE_FIPS_ENDPOINT"},
		{"steering in dev", Identity{Kind: "imds"}, map[string]string{"AWS_PROFILE": "prod"}, true, ""},
		{"imds disabled", Identity{Kind: "imds"}, map[string]string{"AWS_EC2_METADATA_DISABLED": "true"}, false, "AWS_EC2_METADATA_DISABLED"},
		{"platform region", Identity{Kind: "imds"}, map[string]string{"AWS_REGION": "us-east-1", "AWS_STS_REGIONAL_ENDPOINTS": "regional"}, false, ""},
	}
	for _, c := range cases {
		_, err := Credentials(c.id, region, Options{Dev: c.dev, Env: fakeEnv(c.vars, nil)})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
}

// A container-credentials endpoint is asked with the token file's current content, and its
// credentials are cached until they near their expiry.
func TestPodIdentityRetrieval(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"AccessKeyId": "AKID", "SecretAccessKey": "secret", "Token": "session",
			"Expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	}))
	defer srv.Close()
	old := podIdentityURIs
	podIdentityURIs = []string{srv.URL}
	defer func() { podIdentityURIs = old }()
	env := fakeEnv(map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": srv.URL, "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "/t"},
		map[string]string{"/t": "tok-1\n"})
	p, err := Credentials(Identity{Kind: "pod_identity"}, "eu-central-1", Options{Env: env})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		c, err := p.Retrieve(context.Background())
		if err != nil || c.AccessKeyID != "AKID" || c.SessionToken != "session" || !c.CanExpire {
			t.Fatalf("%+v %v", c, err)
		}
	}
	if calls != 1 {
		t.Fatalf("%d endpoint calls, want 1 (cached)", calls)
	}
}

// The HTTP client follows no redirect: a credential endpoint cannot send kista elsewhere.
func TestNoRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed a redirect") }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()
	resp, err := HTTPClient().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// Static key files must be private regular files.
func TestReadSecretFile(t *testing.T) {
	dir := t.TempDir()
	ok, open := filepath.Join(dir, "ok"), filepath.Join(dir, "open")
	if err := os.WriteFile(ok, []byte("AKID\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(open, []byte("AKID\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := readSecretFile(ok); err != nil || string(b) != "AKID\n" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := readSecretFile(open); err == nil {
		t.Fatal("a world-readable key file was read")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(ok, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecretFile(link); err == nil {
		t.Fatal("a symlinked key file was read")
	}
}

// A projected token is a symlink into Kubernetes' timestamped directory, with the volume's mode: it
// is read; an empty token is refused.
func TestProjectedTokenFile(t *testing.T) {
	dir := t.TempDir()
	stamp := filepath.Join(dir, "..2026_10_12_10_00_00.000")
	if err := os.Mkdir(stamp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stamp, "token"), []byte("eyJ.token.sig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(stamp), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "token")
	if err := os.Symlink(filepath.Join("..data", "token"), link); err != nil {
		t.Fatal(err)
	}
	if tok, err := (fileToken{link, readTokenFile}).GetToken(); err != nil || tok != "eyJ.token.sig" {
		t.Fatalf("%q %v", tok, err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (fileToken{empty, readTokenFile}).GetToken(); err == nil {
		t.Fatal("an empty token was used")
	}
	if _, err := (fileToken{dir, readTokenFile}).GetToken(); err == nil {
		t.Fatal("a directory was read as a token")
	}
}
