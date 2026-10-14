// Package aws builds AWS credentials from explicit config (specs 0004 phase 3, 0005 phase 3a): EKS
// Pod Identity, ECS task roles, IRSA, or IMDSv2, optionally followed by an AssumeRole; static key
// files only in development. The SDK's default chain and shared config files are never loaded; the
// platform's own variables are read for the kind chosen, and steering variables are refused.
package aws

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/credentials/endpointcreds"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/hugr-lab/duckdb-extension-repository/internal/secretfile"
)

// Identity is the credential choice.
type Identity struct {
	Kind          string // pod_identity | irsa | imds | ecs | static (dev only)
	PlatformRole  string // irsa: the role the ServiceAccount is bound to; AWS_ROLE_ARN must equal it
	AccessKeyFile string // static
	SecretKeyFile string // static
	AssumeRole    string // optional: an AssumeRole after the base credentials
}

// Kinds are the identity kinds; static is allowed only in development.
var Kinds = []string{"pod_identity", "irsa", "imds", "ecs", "static"}

// Steering are variables that would make an AWS SDK use credentials, files or endpoints the config
// did not choose. kista never reads them, but outside development it refuses to start with one
// set, since whoever set it expects it to apply.
var Steering = []string{"AWS_ACCESS_KEY_ID", "AWS_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_SECRET_KEY", "AWS_SESSION_TOKEN",
	"AWS_CONTAINER_AUTHORIZATION_TOKEN", "AWS_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_CA_BUNDLE",
	"AWS_EC2_METADATA_SERVICE_ENDPOINT", "AWS_EC2_METADATA_SERVICE_ENDPOINT_MODE", "AWS_USE_FIPS_ENDPOINT",
	"AWS_USE_DUALSTACK_ENDPOINT", "AWS_ENDPOINT_URL"}

// steering reports a steering variable set in env (AWS_ENDPOINT_URL_* included).
func steering(environ []string) string {
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if v == "" {
			continue
		}
		for _, s := range Steering {
			if k == s {
				return k
			}
		}
		if strings.HasPrefix(k, "AWS_ENDPOINT_URL_") {
			return k
		}
	}
	return ""
}

var (
	regionRule  = regexp.MustCompile(`^[a-z]{2,4}(-[a-z]+)+-\d{1,2}$`)
	accountRule = regexp.MustCompile(`^\d{12}$`)
	roleRule    = regexp.MustCompile(`^arn:(aws[a-z-]*):iam::(\d{12}):role/[A-Za-z0-9+=,.@_/-]{1,512}$`)
)

// ValidRegion reports whether s has a region's shape.
func ValidRegion(s string) bool { return regionRule.MatchString(s) }

// ValidAccount reports whether s is a 12-digit account id.
func ValidAccount(s string) bool { return accountRule.MatchString(s) }

// partitions maps region prefixes to partitions, longest first; any other region is in aws.
var partitions = []struct{ prefix, partition string }{
	{"us-isob-", "aws-iso-b"}, {"us-isof-", "aws-iso-f"}, {"eu-isoe-", "aws-iso-e"}, {"us-iso-", "aws-iso"},
	{"us-gov-", "aws-us-gov"}, {"cn-", "aws-cn"}, {"eusc-", "aws-eusc"},
}

// Partition returns a region's partition. A wrong guess fails closed: the ARNs kista builds are
// compared with the ones AWS returns.
func Partition(region string) string {
	for _, p := range partitions {
		if strings.HasPrefix(region, p.prefix) {
			return p.partition
		}
	}
	return "aws"
}

// FIPSPartition reports whether the region's partition has FIPS endpoints for KMS and STS.
func FIPSPartition(region string) bool {
	p := Partition(region)
	return p == "aws" || p == "aws-us-gov"
}

// ValidRoleARN reports whether s is an IAM role ARN in the region's partition.
func ValidRoleARN(s, region string) bool {
	m := roleRule.FindStringSubmatch(s)
	return m != nil && m[1] == Partition(region)
}

// HTTPClient is the client for AWS calls and the platform's credential endpoints: no proxy from the
// environment (HTTP_PROXY must not see a credential request or the token it returns), TLS 1.2 or
// later, no redirects.
func HTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// The platform credential endpoints kista accepts (EKS Pod Identity, ECS).
var (
	podIdentityURIs = []string{"http://169.254.170.23/v1/credentials", "http://[fd00:ec2::23]/v1/credentials"}
	ecsHost         = "http://169.254.170.2"
	imdsEndpoint    = "http://169.254.169.254"
)

// Env is what Credentials reads from the process: its environment, and files (tests replace both).
type Env struct {
	Getenv   func(string) string
	Environ  func() []string
	ReadFile func(string) ([]byte, error) // static key files: private regular files
	// ReadToken reads a platform token file (spec 0013: owned by root or kista, any read bits)
	ReadToken func(string) ([]byte, error)
}

func (e Env) defaults() Env {
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.Environ == nil {
		e.Environ = os.Environ
	}
	if e.ReadFile == nil {
		e.ReadFile = readSecretFile
	}
	if e.ReadToken == nil {
		e.ReadToken = readTokenFile
	}
	return e
}

// Options configure Credentials.
type Options struct {
	Dev        bool
	FIPS       bool         // STS's FIPS endpoint
	HTTPClient *http.Client // default HTTPClient()
	Env        Env
	// STSEndpoint replaces STS's endpoint (tests, and the emulator in development); empty in production.
	STSEndpoint string
}

// Credentials builds the credentials for an identity in a region: cached, refreshed before they
// expire. Nothing is fetched here.
func Credentials(id Identity, region string, o Options) (aws.CredentialsProvider, error) {
	env := o.Env.defaults()
	if !ValidRegion(region) {
		return nil, fmt.Errorf("aws: region %q is not a region", region)
	}
	if !o.Dev {
		if v := steering(env.Environ()); v != "" {
			return nil, fmt.Errorf("aws: %s is set; outside profile dev the credentials come from config only", v)
		}
	}
	if id.AssumeRole != "" && !ValidRoleARN(id.AssumeRole, region) {
		return nil, fmt.Errorf("aws: role_arn is not an IAM role ARN in partition %s", Partition(region))
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = HTTPClient()
	}
	stsClient := func(c aws.CredentialsProvider) *sts.Client {
		so := sts.Options{Region: region, HTTPClient: hc, Credentials: c, RetryMaxAttempts: 3}
		if o.FIPS && strings.HasPrefix(region, "us-") { // STS has FIPS endpoints in the US regions only
			so.EndpointOptions.UseFIPSEndpoint = aws.FIPSEndpointStateEnabled
		}
		if o.STSEndpoint != "" {
			so.BaseEndpoint = aws.String(o.STSEndpoint)
		}
		return sts.New(so)
	}
	var base aws.CredentialsProvider
	switch id.Kind {
	case "pod_identity":
		uri, tokenFile := env.Getenv("AWS_CONTAINER_CREDENTIALS_FULL_URI"), env.Getenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE")
		if !oneOf(uri, podIdentityURIs) || tokenFile == "" {
			return nil, errors.New("aws: pod_identity needs the EKS Pod Identity agent's AWS_CONTAINER_CREDENTIALS_FULL_URI and AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE")
		}
		base = endpointcreds.New(uri, func(eo *endpointcreds.Options) {
			eo.HTTPClient = hc
			eo.AuthorizationTokenProvider = fileToken{tokenFile, env.ReadToken}
		})
	case "ecs":
		rel := env.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
		u, err := url.Parse(ecsHost + rel)
		if !strings.HasPrefix(rel, "/") || strings.ContainsAny(rel, "@\\?#%") || strings.Contains(rel, "//") ||
			strings.Contains(rel, "..") || err != nil || u.Host != "169.254.170.2" || u.User != nil || u.Port() != "" {
			return nil, errors.New("aws: ecs needs the task's AWS_CONTAINER_CREDENTIALS_RELATIVE_URI (a path on 169.254.170.2)")
		}
		base = endpointcreds.New(u.String(), func(eo *endpointcreds.Options) { eo.HTTPClient = hc })
	case "irsa":
		role, tokenFile := env.Getenv("AWS_ROLE_ARN"), env.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
		switch {
		case !ValidRoleARN(id.PlatformRole, region):
			return nil, errors.New("aws: irsa needs identity.role_arn, a role in the region's partition")
		case role != id.PlatformRole || tokenFile == "":
			return nil, errors.New("aws: irsa needs AWS_WEB_IDENTITY_TOKEN_FILE, and AWS_ROLE_ARN equal to identity.role_arn")
		}
		base = stscreds.NewWebIdentityRoleProvider(stsClient(aws.AnonymousCredentials{}), role, fileToken{tokenFile, env.ReadToken},
			func(wo *stscreds.WebIdentityRoleOptions) { wo.RoleSessionName = "kista" })
	case "imds":
		if !o.Dev && strings.EqualFold(env.Getenv("AWS_EC2_METADATA_DISABLED"), "true") {
			return nil, errors.New("aws: AWS_EC2_METADATA_DISABLED is set, and identity kind imds uses the instance metadata service")
		}
		// every choice explicit: the IMDS client otherwise reads AWS_EC2_METADATA_* itself
		c := imds.New(imds.Options{Endpoint: imdsEndpoint, EndpointMode: imds.EndpointModeStateIPv4, ClientEnableState: imds.ClientEnabled,
			HTTPClient: hc, EnableFallback: aws.FalseTernary})
		base = ec2rolecreds.New(func(eo *ec2rolecreds.Options) { eo.Client = c })
	case "static":
		if !o.Dev {
			return nil, errors.New("aws: static credentials are allowed only with profile dev")
		}
		if id.AccessKeyFile == "" || id.SecretKeyFile == "" {
			return nil, errors.New("aws: static credentials need access_key_file and secret_key_file")
		}
		base = staticFiles{id.AccessKeyFile, id.SecretKeyFile, env.ReadFile}
	default:
		return nil, fmt.Errorf("aws: identity kind must be one of %s", strings.Join(Kinds, ", "))
	}
	if id.AssumeRole != "" {
		base = stscreds.NewAssumeRoleProvider(stsClient(aws.NewCredentialsCache(base)), id.AssumeRole,
			func(ao *stscreds.AssumeRoleOptions) { ao.RoleSessionName = "kista" })
	}
	// refreshed five minutes (jittered) before they expire, so no request signs with an expiring one
	return aws.NewCredentialsCache(base, func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow, o.ExpiryWindowJitterFrac = 5*time.Minute, 0.5
	}), nil
}

func oneOf(s string, list []string) bool {
	for _, x := range list {
		if s == x {
			return true
		}
	}
	return false
}

// fileToken reads a token file at every use (the platform rotates it).
type fileToken struct {
	path string
	read func(string) ([]byte, error)
}

// GetToken implements endpointcreds.AuthTokenProvider.
func (f fileToken) GetToken() (string, error) {
	b, err := f.read(f.path)
	if err != nil {
		return "", errors.New("aws: the platform's token file could not be read")
	}
	t := strings.TrimSpace(string(b))
	if t == "" || strings.ContainsAny(t, " \t\r\n") {
		return "", errors.New("aws: the platform's token file holds no token")
	}
	return t, nil
}

// GetIdentityToken implements stscreds.IdentityTokenRetriever.
func (f fileToken) GetIdentityToken() ([]byte, error) {
	t, err := f.GetToken()
	return []byte(t), err
}

// staticFiles are development key files, re-read at every retrieval.
type staticFiles struct {
	access, secret string
	read           func(string) ([]byte, error)
}

// staticRefresh is how long development key files are trusted before they are read again: the
// SDK's cache never retrieves credentials that cannot expire.
const staticRefresh = 10 * time.Minute // above the cache's expiry window

func (s staticFiles) Retrieve(context.Context) (aws.Credentials, error) {
	a, err := s.read(s.access)
	if err != nil {
		return aws.Credentials{}, errors.New("aws: access_key_file could not be read")
	}
	k, err := s.read(s.secret)
	if err != nil {
		return aws.Credentials{}, errors.New("aws: secret_key_file could not be read")
	}
	return aws.Credentials{AccessKeyID: strings.TrimSpace(string(a)), SecretAccessKey: strings.TrimSpace(string(k)), Source: "kista-static",
		CanExpire: true, Expires: time.Now().Add(staticRefresh)}, nil
}

// readTokenFile reads a platform token file (EKS Pod Identity, IRSA): a token (spec 0013), which
// Kubernetes projects as a link into a timestamped directory, with the mode the webhook's volume
// sets; at most 64 KiB.
func readTokenFile(path string) ([]byte, error) {
	b, err := secretfile.Read(path, secretfile.Token, secretfile.SmallLimit)
	if err != nil {
		return nil, fmt.Errorf("aws: %w", err)
	}
	return b, nil
}

// readSecretFile reads a static credential file (dev): a secret file (spec 0013).
func readSecretFile(path string) ([]byte, error) {
	b, err := secretfile.Read(path, secretfile.Secret, secretfile.SmallLimit)
	if err != nil {
		return nil, fmt.Errorf("aws: %w", err)
	}
	return b, nil
}

// S3Host reports whether host (no port) is an S3 endpoint of AWS in the region (spec 0005 phase
// 3a): s3[-fips][.dualstack].<region>.amazonaws.com in the aws and aws-us-gov partitions,
// s3[.dualstack].<region>.amazonaws.com.cn in aws-cn. AWS credentials are never sent to another host.
func S3Host(host, region string) bool {
	host = strings.ToLower(host)
	if net.ParseIP(host) != nil || !ValidRegion(region) {
		return false
	}
	var suffix string
	forms := []string{"s3.", "s3.dualstack."}
	switch Partition(region) {
	case "aws", "aws-us-gov":
		suffix = ".amazonaws.com"
		forms = append(forms, "s3-fips.", "s3-fips.dualstack.")
	case "aws-cn":
		suffix = ".amazonaws.com.cn"
	default:
		return false
	}
	for _, p := range forms {
		if host == p+region+suffix {
			return true
		}
	}
	return false
}
