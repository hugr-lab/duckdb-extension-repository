// Package config loads kista's configuration: a YAML file plus KISTA_<PATH> environment overrides
// (spec 0003). Unknown keys and unknown KISTA_ variables are refused; settings that switch off a
// guard can only be set in the file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	awscloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/aws"
	gcpcloud "github.com/hugr-lab/duckdb-extension-repository/internal/cloud/gcp"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"go.yaml.in/yaml/v3"
)

// Profiles.
const (
	ProfileProd = "prod"
	ProfileDev  = "dev"
)

// Config is kista's configuration.
type Config struct {
	Profile string `yaml:"profile" kista:"fileonly"`
	Store   Store  `yaml:"store"`
	Azure   Azure  `yaml:"azure"`
	// AWS and GCP are the service's own identities on those clouds, for what is not a key source or
	// a blob domain: the database login (spec 0020).
	AWS      AWS      `yaml:"aws" kista:"fileonly"`
	GCP      GCP      `yaml:"gcp" kista:"fileonly"`
	Rotation Rotation `yaml:"rotation"`
	Signers  Signers  `yaml:"signers" kista:"fileonly"`
	Blob     Blob     `yaml:"blob" kista:"fileonly"`
	Serve    Serve    `yaml:"serve"`
	Egress   Egress   `yaml:"egress" kista:"fileonly"`
	Auth     Auth     `yaml:"auth" kista:"fileonly"`
	Publish  Publish  `yaml:"publish" kista:"fileonly"`
	// Upstreams configures upstream runs (spec 0009).
	Upstreams Upstreams `yaml:"upstreams" kista:"fileonly"`
	// Events configures the event buffer (spec 0010).
	Events Events `yaml:"events" kista:"fileonly"`
	// Statistics configures download statistics (spec 0010 phase 2a).
	Statistics Statistics `yaml:"statistics" kista:"fileonly"`
	// Telemetry configures what OpenTelemetry metrics may name (spec 0010 phase 2b); where they go is
	// the standard OTEL_* environment's.
	Telemetry Telemetry `yaml:"telemetry" kista:"fileonly"`
	// GC configures storage garbage collection (spec 0016).
	GC GC `yaml:"gc" kista:"fileonly"`
	// UI configures the administration console (spec 0015).
	UI UI `yaml:"ui" kista:"fileonly"`
	// Attachments configures documents about bodies (spec 0011).
	Attachments Attachments `yaml:"attachments" kista:"fileonly"`
	// Tenants configures deleting tenants (spec 0019).
	Tenants Tenants `yaml:"tenants" kista:"fileonly"`
}

// Tenants holds the tenant lifecycle settings.
type Tenants struct {
	// MinSuspended is how long a tenant stays suspended before it may be deleted (spec 0019).
	MinSuspended time.Duration `yaml:"min_suspended"`
}

// MinSuspendedFloor is the least tenants.min_suspended may be outside the dev profile.
const MinSuspendedFloor = 24 * time.Hour

// Store selects and configures the metadata database.
type Store struct {
	Kind         string `yaml:"kind"` // postgres | sqlserver | sqlite
	DSN          string `yaml:"dsn"`
	Login        Login  `yaml:"login"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	Migrate      string `yaml:"migrate"` // auto | check
	SQLite       SQLite `yaml:"sqlite"`
	// CAPEM is the PostgreSQL server's CA, inline (spec 0020): public data, for platforms that
	// mount no file; it replaces the DSN's sslrootcert. File-only: a trust anchor is no override.
	CAPEM string `yaml:"ca_pem" kista:"fileonly"`
}

// Login says how the database password or token is obtained.
type Login struct {
	Kind         string `yaml:"kind"` // password | entra | aws | gcp
	PasswordEnv  string `yaml:"password_env"`
	PasswordFile string `yaml:"password_file"`
}

// SQLite configures a SQLite store.
type SQLite struct {
	Path string `yaml:"path"`
}

// Azure configures the service's Azure identity.
type Azure struct {
	Identity Identity `yaml:"identity"`
	Cloud    string   `yaml:"cloud"` // public (default) | china | usgov; for the entra database login
}

// AWS is the service's AWS identity outside key sources and blob domains (spec 0020).
type AWS struct {
	Region   string    `yaml:"region"`
	Identity *Identity `yaml:"identity"`
}

// GCP is the service's Google Cloud identity outside key sources and blob domains (spec 0020).
type GCP struct {
	GCPCreds `yaml:",inline"`
}

// Identity is an Azure credential choice.
type Identity struct {
	// Azure: managed | workload | default (dev only); AWS: pod_identity | irsa | imds | ecs | static (dev only)
	Kind     string `yaml:"kind" kista:"fileonly"`
	ClientID string `yaml:"client_id"` // Azure
	TenantID string `yaml:"tenant_id"` // Azure
	// AWS static (dev only): key files, mode 0600 or narrower
	AccessKeyFile string `yaml:"access_key_file" kista:"fileonly"`
	SecretKeyFile string `yaml:"secret_key_file" kista:"fileonly"`
	// AWS irsa: the role the ServiceAccount is bound to; AWS_ROLE_ARN must equal it
	RoleARN string `yaml:"role_arn" kista:"fileonly"`
}

// GCPCreds is how kista reaches Google Cloud (spec 0004 phase 4): the service account it acts as,
// and a credentials file (absent: the metadata server).
type GCPCreds struct {
	Principal       string `yaml:"principal"`        // the service account email; required outside dev
	CredentialsFile string `yaml:"credentials_file"` // an external_account file; a service_account key in dev only
	EndpointURL     string `yaml:"endpoint_url"`     // the tests' fake: profile dev, loopback, no credentials file
}

// GCPKMSSource is a Cloud KMS key ring (spec 0004 phase 4).
type GCPKMSSource struct {
	KeyRing    string `yaml:"key_ring"` // projects/<project id>/locations/<location>/keyRings/<ring>
	RequireHSM *bool  `yaml:"require_hsm"`
	GCPCreds   `yaml:",inline"`
}

// AWSKMSSource is an AWS KMS account and region (spec 0004 phase 3).
type AWSKMSSource struct {
	Region           string `yaml:"region"`
	Account          string `yaml:"account"`
	RoleARN          string `yaml:"role_arn"` // optional AssumeRole, for a key in another account
	AllowMultiRegion bool   `yaml:"allow_multi_region"`
	FIPS             bool   `yaml:"fips"`         // the FIPS endpoints of KMS and STS
	EndpointURL      string `yaml:"endpoint_url"` // the emulator: profile dev, loopback only
}

// Rotation holds the key-lifecycle minimums.
type Rotation struct {
	MinTrusted time.Duration `yaml:"min_trusted"`
	MinDemoted time.Duration `yaml:"min_demoted"`
}

// Signers configures where signing keys may come from (specs 0003, 0004). The whole block is
// file-only.
type Signers struct {
	AllowFile bool     `yaml:"allow_file"`
	FileDir   string   `yaml:"file_dir"`
	Sources   []Source `yaml:"sources"`
}

// Source is a named key source (spec 0004): a vault or KMS the administrator configured. A signer
// reference <source>:<key> picks a key inside it; the reference never carries a host.
type Source struct {
	Name           string         `yaml:"name"`
	Kind           string         `yaml:"kind"` // vault | azurekv | awskms | gcpkms
	Allow          []string       `yaml:"allow"`
	MaxConcurrency int            `yaml:"max_concurrency"`
	HealthKey      string         `yaml:"health_key"`
	Vault          *VaultSource   `yaml:"vault"`
	AzureKV        *AzureKVSource `yaml:"azurekv"`
	AWSKMS         *AWSKMSSource  `yaml:"awskms"` // phase 3
	GCPKMS         *GCPKMSSource  `yaml:"gcpkms"` // phase 4
	Identity       *Identity      `yaml:"identity"`
	Recheck        time.Duration  `yaml:"recheck"` // how often key properties are re-read; default and maximum 10m
	Timeout        time.Duration  `yaml:"timeout"` // per call; default 10s, 1s..60s
}

// VaultSource is a HashiCorp Vault or OpenBao Transit mount.
type VaultSource struct {
	Address      string    `yaml:"address"`
	Namespace    string    `yaml:"namespace"`
	Mount        string    `yaml:"mount"`
	CAFile       string    `yaml:"ca_file"`
	Auth         VaultAuth `yaml:"auth"`
	SoftwareKeys bool      `yaml:"software_keys"` // required outside dev: Transit keys are software keys
}

// AzureKVSource is an Azure Key Vault or Managed HSM.
type AzureKVSource struct {
	Vault      string `yaml:"vault"`       // the vault name: https://<vault>.vault.<cloud suffix>
	ManagedHSM string `yaml:"managed_hsm"` // or a Managed HSM name: https://<hsm>.managedhsm.<suffix>
	Cloud      string `yaml:"cloud"`       // public (default) | china | usgov
	RequireHSM *bool  `yaml:"require_hsm"` // default true outside dev: RSA-HSM keys only
}

// VaultAuth says how kista logs in to Vault: no static tokens in config, no AppRole.
type VaultAuth struct {
	Kind      string `yaml:"kind"`       // kubernetes | jwt | token_file
	Mount     string `yaml:"mount"`      // auth mount path; default kubernetes or jwt
	Role      string `yaml:"role"`       // kubernetes and jwt
	TokenFile string `yaml:"token_file"` // the projected service-account token, the JWT, or Vault Agent's token
}

// SourceKinds are the backends; reservedSourceNames cannot name a source.
var (
	SourceKinds         = []string{"vault", "azurekv", "awskms", "gcpkms"}
	reservedSourceNames = map[string]bool{"file": true, "vault": true, "azurekv": true, "awskms": true,
		"gcpkms": true, "pkcs11": true, "http": true, "https": true, "arn": true, "projects": true}
	sourceName     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
	vaultNamespace = regexp.MustCompile(`^[A-Za-z0-9_/-]*$`)
)

// RotationFloor is the least either rotation minimum may be outside the dev profile.
const RotationFloor = 24 * time.Hour

// Default returns the defaults.
func Default() Config {
	return Config{
		Profile:  ProfileProd,
		Store:    Store{MaxOpenConns: 10, Migrate: "auto", Login: Login{Kind: "password"}},
		Rotation: Rotation{MinTrusted: 7 * 24 * time.Hour, MinDemoted: 7 * 24 * time.Hour},
		Tenants:  Tenants{MinSuspended: 7 * 24 * time.Hour},
	}
}

// envPrefix is the prefix of every override variable.
const envPrefix = "KISTA_"

// reservedEnv are KISTA_ variables that are not settings: test and harness switches.
var reservedEnvPrefixes = []string{"KISTA_TEST_", "KISTA_E2E_", "KISTA_LIVE_"}

// MaxEnvConfig bounds a configuration read from a variable (spec 0020).
const MaxEnvConfig = 1 << 20

// envConfigName is the name a variable holding the configuration may have.
var envConfigName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)

// Load reads the configuration and applies KISTA_ overrides from env. path is a file, or
// env:<NAME> for a variable of env holding the YAML (spec 0020: platforms that mount no file), or
// empty for the defaults only. env is os.Environ() in production; tests pass their own.
func Load(path string, env []string) (Config, error) {
	cfg := Default()
	var data []byte
	from, configVar := path, ""
	if name, ok := strings.CutPrefix(path, "env:"); ok {
		if !envConfigName.MatchString(name) {
			return Config{}, errors.New("config: env:<NAME> needs a variable name ([A-Z_][A-Z0-9_]*)")
		}
		v, found := "", false
		for _, kv := range env { // the first, as os.Getenv
			if k, val, _ := strings.Cut(kv, "="); k == name {
				v, found = val, true
				break
			}
		}
		switch {
		case !found || strings.TrimSpace(v) == "":
			return Config{}, fmt.Errorf("config: the variable %s is not set", name)
		case len(v) > MaxEnvConfig:
			return Config{}, fmt.Errorf("config: the variable %s is larger than %d bytes", name, MaxEnvConfig)
		}
		data, from, configVar = []byte(v), "the variable "+name, name
	} else if path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
	}
	if data != nil {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) { // an empty file keeps the defaults
			return Config{}, fmt.Errorf("config: %s: %s", from, scrub(err))
		}
	}
	if err := applyEnv(&cfg, env, configVar); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// scrub drops quoted values from YAML errors, so a misplaced secret is never echoed.
func scrub(err error) string {
	msg := err.Error()
	var b strings.Builder
	in := false
	for _, r := range msg {
		if r == '`' || r == '"' {
			if !in {
				b.WriteString("<value>")
			}
			in = !in
			continue
		}
		if !in {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// setting is one leaf of the config tree.
type setting struct {
	env      string
	path     string
	fileOnly bool
	field    reflect.Value
}

func settings(v reflect.Value, prefix, path string, fileOnly bool, out *[]setting) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		fo := fileOnly || f.Tag.Get("kista") == "fileonly"
		env := prefix + strings.ToUpper(name)
		p := name
		if path != "" {
			p = path + "." + name
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Struct && fv.Type() != reflect.TypeFor[time.Duration]() {
			settings(fv, env+"__", p, fo, out)
			continue
		}
		*out = append(*out, setting{env: env, path: p, fileOnly: fo, field: fv})
	}
}

func applyEnv(cfg *Config, env []string, configVar string) error {
	var all []setting
	settings(reflect.ValueOf(cfg).Elem(), envPrefix, "", false, &all)
	byEnv := map[string]setting{}
	for _, s := range all {
		byEnv[s.env] = s
	}
	var errs, unknown []string
	if _, ok := byEnv[configVar]; ok {
		return fmt.Errorf("config: %s is a kista setting; name the variable holding the configuration otherwise", configVar)
	}
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, envPrefix) || reserved(name) {
			continue
		}
		s, ok := byEnv[name]
		if !ok {
			unknown = append(unknown, name)
			continue
		}
		if s.fileOnly {
			errs = append(errs, fmt.Sprintf("%s (%s) can only be set in the config file", name, s.path))
			continue
		}
		if err := set(s.field, val); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", name, err))
		}
	}
	// a variable named by password_env (after the overrides) holds a secret, and the one -config
	// env: names holds the configuration itself: neither is a setting
	for _, name := range unknown {
		if name != cfg.Store.Login.PasswordEnv && name != configVar {
			errs = append(errs, fmt.Sprintf("%s is not a kista setting", name))
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

func reserved(name string) bool {
	for _, p := range reservedEnvPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// set parses an environment value into a field. Errors never include the value.
func set(f reflect.Value, val string) error {
	switch {
	case f.Type() == reflect.TypeFor[time.Duration]():
		d, err := time.ParseDuration(val)
		if err != nil {
			return errors.New("not a duration")
		}
		f.SetInt(int64(d))
	case f.Kind() == reflect.String:
		f.SetString(val)
	case f.Kind() == reflect.Int:
		n, err := strconv.Atoi(val)
		if err != nil {
			return errors.New("not an integer")
		}
		f.SetInt(int64(n))
	case f.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(val)
		if err != nil {
			return errors.New("not a boolean")
		}
		f.SetBool(b)
	default:
		return errors.New("cannot be set from the environment")
	}
	return nil
}

// Validate checks the whole configuration.
func (c Config) Validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }
	if c.Profile != ProfileProd && c.Profile != ProfileDev {
		bad("profile must be prod or dev")
	}
	dev := c.Profile == ProfileDev
	switch c.Store.Kind {
	case "sqlite":
		if c.Store.SQLite.Path == "" {
			bad("store.sqlite.path is required")
		}
	case "postgres", "sqlserver":
		if c.Store.DSN == "" {
			bad("store.dsn is required")
			break
		}
		if store.DSNHasPassword(c.Store.DSN) {
			bad("store.dsn must not contain a password; use store.login")
		}
		if dev {
			// judged from the driver's own parsing: every host, fallbacks and failover partner included
			if local, err := store.DSNIsLocal(c.Store.Kind, c.Store.DSN); err != nil || !local {
				bad("profile dev refuses a store that is not SQLite or on a loopback host")
			}
		}
	case "":
		bad("store.kind is required")
	default:
		bad("store.kind must be postgres, sqlserver or sqlite")
	}
	if c.Store.Migrate != "auto" && c.Store.Migrate != "check" {
		bad("store.migrate must be auto or check")
	}
	pwSet := c.Store.Login.PasswordEnv != "" || c.Store.Login.PasswordFile != ""
	switch c.Store.Login.Kind {
	case "password":
		if c.Store.Login.PasswordEnv != "" && c.Store.Login.PasswordFile != "" {
			bad("store.login: set password_env or password_file, not both")
		}
	case "entra":
		if c.Store.Kind == "sqlite" {
			bad("store.login entra needs postgres or sqlserver")
		}
		if pwSet {
			bad("store.login entra takes no password_env or password_file: the token is the password")
		}
	case "aws", "gcp":
		validateTokenLogin(bad, c)
	default:
		bad("store.login.kind must be password, entra, aws or gcp")
	}
	if c.Store.CAPEM != "" {
		switch {
		case c.Store.Kind != "postgres":
			bad("store.ca_pem is for postgres")
		case !store.ValidCAPEM(c.Store.CAPEM):
			bad("store.ca_pem must hold PEM certificates, and nothing else")
		case store.DSNHasRootCert(c.Store.DSN):
			bad("store.ca_pem and the DSN's sslrootcert do not go together: give the CA once")
		default:
			if t, err := store.PostgresTarget(c.Store.DSN); err == nil && !t.TLS {
				bad("store.ca_pem is given, but the DSN reaches no host over TLS")
			}
		}
	}
	if c.AWS != (AWS{}) {
		if !awscloud.ValidRegion(c.AWS.Region) {
			bad("aws.region is required, a region such as eu-central-1")
		}
		validateAWSIdentity(bad, "aws", c.AWS.Identity, c.AWS.Region, dev)
	}
	if c.GCP != (GCP{}) {
		validateGCPCreds(bad, "gcp", c.GCP.GCPCreds, dev)
	}
	switch c.Azure.Cloud {
	case "", "public", "china", "usgov":
	default:
		bad("azure.cloud must be public, china or usgov")
	}
	if id := c.Azure.Identity; id.RoleARN != "" || id.AccessKeyFile != "" || id.SecretKeyFile != "" {
		bad("azure.identity: role_arn and the key files are AWS's")
	}
	switch c.Azure.Identity.Kind {
	case "", "managed":
	case "workload":
		if !dev && (c.Azure.Identity.ClientID == "" || c.Azure.Identity.TenantID == "") {
			bad("azure.identity: workload identity needs client_id and tenant_id")
		}
	case "default":
		if !dev {
			bad("azure.identity.kind default (the developer credential chain) is allowed only with profile dev")
		}
	default:
		bad("azure.identity.kind must be managed, workload or default")
	}
	if !dev {
		if c.Rotation.MinTrusted < RotationFloor || c.Rotation.MinDemoted < RotationFloor {
			bad("rotation minimums cannot be below %s outside profile dev", RotationFloor)
		}
	}
	if c.Rotation.MinTrusted < 0 || c.Rotation.MinDemoted < 0 {
		bad("rotation minimums cannot be negative")
	}
	if c.Tenants.MinSuspended < 0 || !dev && c.Tenants.MinSuspended < MinSuspendedFloor {
		bad("tenants.min_suspended cannot be negative, nor below %s outside profile dev", MinSuspendedFloor)
	}
	if c.Signers.FileDir != "" && !strings.HasPrefix(c.Signers.FileDir, "/") {
		bad("signers.file_dir must be an absolute path")
	}
	seen := map[string]bool{}
	for i, src := range c.Signers.Sources {
		where := fmt.Sprintf("signers.sources[%d]", i)
		if !sourceName.MatchString(src.Name) || reservedSourceNames[src.Name] {
			bad("%s.name must match [a-z][a-z0-9-]{0,15} and not be reserved", where)
		}
		if seen[src.Name] {
			bad("%s: source %s is defined twice", where, src.Name)
		}
		seen[src.Name] = true
		if len(src.Allow) == 0 {
			bad("%s.allow is empty: list key-name prefixes, or \"*\"", where)
		}
		for _, a := range src.Allow {
			if a == "" {
				bad("%s.allow: use \"*\" for any key, not an empty prefix", where)
			}
		}
		if src.MaxConcurrency < 0 || src.Recheck < 0 || src.Recheck > 10*time.Minute {
			bad("%s: max_concurrency cannot be negative and recheck must be within 0..10m", where)
		}
		if src.Timeout != 0 && (src.Timeout < time.Second || src.Timeout > time.Minute) {
			bad("%s.timeout must be within 1s..60s", where)
		}
		if src.Kind == "vault" && src.Identity != nil {
			bad("%s.identity is not used by kind vault (it logs in through vault.auth)", where)
		}
		blocks := 0
		for _, set := range []bool{src.Vault != nil, src.AzureKV != nil, src.AWSKMS != nil, src.GCPKMS != nil} {
			if set {
				blocks++
			}
		}
		switch src.Kind {
		case "vault":
			if src.Vault == nil || blocks != 1 {
				bad("%s: kind vault needs exactly one block, vault", where)
				continue
			}
			validateVault(bad, where, *src.Vault, dev)
		case "azurekv":
			for _, a := range src.Allow {
				if a != strings.ToLower(a) {
					bad("%s.allow: Key Vault names are matched lowercased; write prefixes in lowercase", where)
				}
			}
			if src.AzureKV == nil || blocks != 1 {
				bad("%s: kind azurekv needs exactly one block, azurekv", where)
				continue
			}
			validateAzureKV(bad, where, *src.AzureKV, src.Identity, dev)
		case "awskms":
			if src.AWSKMS == nil || blocks != 1 {
				bad("%s: kind awskms needs exactly one block, awskms", where)
				continue
			}
			validateAWSKMS(bad, where, *src.AWSKMS, src.Identity, src.Allow, dev)
		case "gcpkms":
			if src.GCPKMS == nil || blocks != 1 {
				bad("%s: kind gcpkms needs exactly one block, gcpkms", where)
				continue
			}
			if src.Identity != nil {
				bad("%s.identity is not used by kind gcpkms (principal and credentials_file are)", where)
			}
			if !gcpcloud.ValidKeyRing(src.GCPKMS.KeyRing) {
				bad("%s.gcpkms.key_ring must be projects/<project id>/locations/<location>/keyRings/<ring>", where)
			}
			if r := src.GCPKMS.RequireHSM; r != nil && !*r && !dev {
				bad("%s.gcpkms.require_hsm: false is allowed only with profile dev", where)
			}
			validateGCPCreds(bad, where+".gcpkms", src.GCPKMS.GCPCreds, dev)
		default:
			bad("%s.kind must be one of %s", where, strings.Join(SourceKinds, ", "))
		}
	}
	validateBlob(bad, c)
	validateServe(bad, c)
	validateEgress(bad, c)
	validateAuth(bad, c)
	validatePublish(bad, c)
	validateUpstreams(bad, c)
	validateEvents(bad, c)
	validateStatistics(bad, c)
	validateTelemetry(bad, c)
	validateGC(bad, c)
	validateAttachments(bad, c)
	validateUI(bad, c)
	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

var azureName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{1,22}[A-Za-z0-9]$`)

func validateAzureKV(bad func(string, ...any), where string, a AzureKVSource, id *Identity, dev bool) {
	switch {
	case (a.Vault == "") == (a.ManagedHSM == ""):
		bad("%s.azurekv: set exactly one of vault and managed_hsm", where)
	case a.Vault != "" && (!azureName.MatchString(a.Vault) || strings.Contains(a.Vault, "--")),
		a.ManagedHSM != "" && (!azureName.MatchString(a.ManagedHSM) || strings.Contains(a.ManagedHSM, "--")):
		bad("%s.azurekv: a vault or Managed HSM name is 3-24 letters, digits and dashes", where)
	}
	switch a.Cloud {
	case "", "public", "china", "usgov":
	default:
		bad("%s.azurekv.cloud must be public, china or usgov", where)
	}
	if a.RequireHSM != nil && !*a.RequireHSM && !dev {
		bad("%s.azurekv.require_hsm: false is allowed only with profile dev", where)
	}
	if id == nil {
		bad("%s.identity is required for kind azurekv", where)
		return
	}
	if id.AccessKeyFile != "" || id.SecretKeyFile != "" || id.RoleARN != "" {
		bad("%s.identity: key files and role_arn are AWS's", where)
	}
	switch id.Kind {
	case "managed":
	case "workload":
		if !dev && (id.ClientID == "" || id.TenantID == "") {
			bad("%s.identity: workload identity needs client_id and tenant_id", where)
		}
	case "default":
		if !dev {
			bad("%s.identity.kind default (the developer chain) is allowed only with profile dev", where)
		}
	default:
		bad("%s.identity.kind must be managed or workload", where)
	}
}

func validateAWSKMS(bad func(string, ...any), where string, a AWSKMSSource, id *Identity, allow []string, dev bool) {
	if !awscloud.ValidRegion(a.Region) {
		bad("%s.awskms.region is required, a region such as eu-central-1", where)
	}
	if !awscloud.ValidAccount(a.Account) {
		bad("%s.awskms.account is required, 12 digits", where)
	}
	if a.RoleARN != "" && !awscloud.ValidRoleARN(a.RoleARN, a.Region) {
		bad("%s.awskms.role_arn must be an IAM role ARN in the region's partition", where)
	}
	if a.FIPS && !awscloud.FIPSPartition(a.Region) {
		bad("%s.awskms.fips: the region's partition has no FIPS endpoints (aws and aws-us-gov do)", where)
	}
	if a.FIPS && a.EndpointURL != "" {
		bad("%s.awskms: fips and endpoint_url do not go together", where)
	}
	if a.EndpointURL != "" && (!dev || !loopbackURL(a.EndpointURL)) {
		bad("%s.awskms.endpoint_url is allowed only with profile dev, on loopback (the emulator)", where)
	}
	if a.EndpointURL != "" && (id == nil || id.Kind != "static") {
		bad("%s.awskms.endpoint_url goes with identity kind static: a platform's token never goes to the emulator", where)
	}
	for _, x := range allow {
		if x != "*" && !awsKeyID.MatchString(x) || strings.HasPrefix(x, "mrk-") && !a.AllowMultiRegion {
			bad("%s.allow: AWS key ids carry no meaning as prefixes; list key ids (mrk- ids with allow_multi_region), or * (the purpose tag guards it)", where)
		}
	}
	validateAWSIdentity(bad, where, id, a.Region, dev)
}

var awsKeyID = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|mrk-[0-9a-f]{32})$`)

// rdsHost is an RDS or Aurora endpoint, with its region.
var rdsHost = regexp.MustCompile(`\.([a-z]{2}(?:-[a-z]+)+-\d)\.rds\.amazonaws\.com(?:\.cn)?\.?$`)

// validateTokenLogin checks the aws and gcp database logins (spec 0020): PostgreSQL, one host (a
// token is bound to it), no password setting, and the identity the token comes from.
func validateTokenLogin(bad func(string, ...any), c Config) {
	kind := c.Store.Login.Kind
	if c.Store.Kind != "postgres" {
		bad("store.login %s needs postgres", kind)
		return
	}
	if c.Store.Login.PasswordEnv != "" || c.Store.Login.PasswordFile != "" {
		bad("store.login %s takes no password_env or password_file: the token is the password", kind)
	}
	t, err := store.PostgresTarget(c.Store.DSN)
	if err != nil {
		return // store.dsn's own check reports it
	}
	if t.Hosts != 1 {
		bad("store.login %s needs a DSN with one host: a token is bound to the host it was made for", kind)
	}
	switch kind {
	case "aws":
		if c.AWS.Region == "" || c.AWS.Identity == nil {
			bad("store.login aws needs aws.region and aws.identity")
		}
		// a token is signed for the database's region: an RDS endpoint names it
		if m := rdsHost.FindStringSubmatch(strings.ToLower(t.Host)); m != nil && m[1] != c.AWS.Region {
			bad("store.login aws: the database is in %s, aws.region is %s; the token is signed for aws.region", m[1], c.AWS.Region)
		}
	case "gcp":
		switch {
		case c.GCP.Principal == "":
			bad("store.login gcp needs gcp.principal, the service account kista logs in as")
		case t.User != strings.TrimSuffix(c.GCP.Principal, ".gserviceaccount.com"):
			bad("store.login gcp: the DSN's user must be gcp.principal without .gserviceaccount.com (Cloud SQL's name for the IAM user)")
		}
	}
}

// validateGCPCreds checks how kista reaches Google Cloud (specs 0004 phase 4, 0005 phase 3b).
func validateGCPCreds(bad func(string, ...any), where string, c GCPCreds, dev bool) {
	switch {
	case c.Principal == "" && !dev:
		bad("%s.principal is required outside profile dev: the service account kista acts as", where)
	case c.Principal != "" && !gcpcloud.ValidPrincipal(c.Principal):
		bad("%s.principal must be a service account email (<name>@<project>.iam.gserviceaccount.com)", where)
	}
	if c.CredentialsFile != "" && !filepath.IsAbs(c.CredentialsFile) {
		bad("%s.credentials_file must be an absolute path", where)
	}
	if c.EndpointURL != "" && (!dev || !loopbackURL(c.EndpointURL) || c.CredentialsFile != "") {
		bad("%s.endpoint_url is allowed only with profile dev, on loopback, without credentials_file (the tests' fake)", where)
	}
}

// validateAWSIdentity checks an AWS identity (spec 0004 phase 3; spec 0005 phase 3a).
func validateAWSIdentity(bad func(string, ...any), where string, id *Identity, region string, dev bool) {
	if id == nil {
		bad("%s.identity is required: pod_identity, irsa, imds or ecs", where)
		return
	}
	if id.ClientID != "" || id.TenantID != "" {
		bad("%s.identity: client_id and tenant_id are Azure's", where)
	}
	if id.Kind == "irsa" && !awscloud.ValidRoleARN(id.RoleARN, region) {
		bad("%s.identity: irsa needs role_arn, the IAM role (in the region's partition) the ServiceAccount is bound to", where)
	}
	if id.Kind != "irsa" && id.RoleARN != "" {
		bad("%s.identity.role_arn is only for irsa", where)
	}
	switch id.Kind {
	case "pod_identity", "irsa", "imds", "ecs":
		if id.AccessKeyFile != "" || id.SecretKeyFile != "" {
			bad("%s.identity: key files are only for kind static", where)
		}
	case "static":
		if !dev {
			bad("%s.identity.kind static is allowed only with profile dev", where)
		}
		if !filepath.IsAbs(id.AccessKeyFile) || !filepath.IsAbs(id.SecretKeyFile) {
			bad("%s.identity: static needs absolute access_key_file and secret_key_file", where)
		}
	default:
		bad("%s.identity.kind must be pod_identity, irsa, imds or ecs", where)
	}
}

// loopbackURL reports whether s is an http(s) URL on a loopback host.
func loopbackURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func validateVault(bad func(string, ...any), where string, v VaultSource, dev bool) {
	u, err := url.Parse(v.Address)
	switch {
	case err != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.User != nil:
		bad("%s.vault.address must be a scheme://host[:port] URL", where)
	case u.Scheme == "http":
		if !dev || !store.IsLoopbackHost(u.Hostname()) {
			bad("%s.vault.address: plain http only on loopback with profile dev", where)
		}
	case u.Scheme != "https":
		bad("%s.vault.address must be https", where)
	}
	if !vaultNamespace.MatchString(v.Namespace) || strings.Contains(v.Namespace, "..") {
		bad("%s.vault.namespace may contain only letters, digits, _, - and /", where)
	}
	if v.Mount == "" || strings.ContainsAny(v.Mount, "?#%") || strings.Contains(v.Mount, "..") || strings.HasPrefix(v.Mount, "/") {
		bad("%s.vault.mount must be a relative mount path", where)
	}
	switch v.Auth.Kind {
	case "kubernetes", "jwt":
		if v.Auth.Role == "" {
			bad("%s.vault.auth.role is required for %s", where, v.Auth.Kind)
		}
	case "token_file":
	default:
		bad("%s.vault.auth.kind must be kubernetes, jwt or token_file", where)
	}
	if !strings.HasPrefix(v.Auth.TokenFile, "/") {
		bad("%s.vault.auth.token_file must be an absolute path", where)
	}
	if !dev && !v.SoftwareKeys {
		bad("%s.vault.software_keys must be true outside profile dev: Transit keys are software keys", where)
	}
}

// FileSignersAllowed reports whether file: signer references may be used.
func (c Config) FileSignersAllowed() bool {
	return c.Signers.FileDir != "" && (c.Profile == ProfileDev || c.Signers.AllowFile)
}
