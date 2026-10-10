// Package config loads kista's configuration: a YAML file plus KISTA_<PATH> environment overrides
// (spec 0003). Unknown keys and unknown KISTA_ variables are refused; settings that switch off a
// guard can only be set in the file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
	Profile  string   `yaml:"profile" kista:"fileonly"`
	Store    Store    `yaml:"store"`
	Azure    Azure    `yaml:"azure"`
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
}

// Store selects and configures the metadata database.
type Store struct {
	Kind         string `yaml:"kind"` // postgres | sqlserver | sqlite
	DSN          string `yaml:"dsn"`
	Login        Login  `yaml:"login"`
	MaxOpenConns int    `yaml:"max_open_conns"`
	Migrate      string `yaml:"migrate"` // auto | check
	SQLite       SQLite `yaml:"sqlite"`
}

// Login says how the database password or token is obtained.
type Login struct {
	Kind         string `yaml:"kind"` // password | entra
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

// Identity is an Azure credential choice.
type Identity struct {
	Kind     string `yaml:"kind" kista:"fileonly"` // managed | workload | default (dev only)
	ClientID string `yaml:"client_id"`
	TenantID string `yaml:"tenant_id"`
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
	AWSKMS         *yaml.Node     `yaml:"awskms"` // phase 3
	GCPKMS         *yaml.Node     `yaml:"gcpkms"` // phase 4
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
	}
}

// envPrefix is the prefix of every override variable.
const envPrefix = "KISTA_"

// reservedEnv are KISTA_ variables that are not settings: test and harness switches.
var reservedEnvPrefixes = []string{"KISTA_TEST_", "KISTA_E2E_", "KISTA_LIVE_"}

// Load reads the file at path (empty: defaults only) and applies KISTA_ overrides from env. env is
// os.Environ() in production; tests pass their own.
func Load(path string, env []string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) { // an empty file keeps the defaults
			return Config{}, fmt.Errorf("config: %s: %s", path, scrub(err))
		}
	}
	if err := applyEnv(&cfg, env); err != nil {
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

func applyEnv(cfg *Config, env []string) error {
	var all []setting
	settings(reflect.ValueOf(cfg).Elem(), envPrefix, "", false, &all)
	byEnv := map[string]setting{}
	for _, s := range all {
		byEnv[s.env] = s
	}
	var errs, unknown []string
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
	// a variable named by password_env (after the overrides) holds a secret, not a setting
	for _, name := range unknown {
		if name != cfg.Store.Login.PasswordEnv {
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
	switch c.Store.Login.Kind {
	case "password":
		if c.Store.Login.PasswordEnv != "" && c.Store.Login.PasswordFile != "" {
			bad("store.login: set password_env or password_file, not both")
		}
	case "entra":
		if c.Store.Kind == "sqlite" {
			bad("store.login entra needs postgres or sqlserver")
		}
	default:
		bad("store.login.kind must be password or entra")
	}
	switch c.Azure.Cloud {
	case "", "public", "china", "usgov":
	default:
		bad("azure.cloud must be public, china or usgov")
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
		case "awskms", "gcpkms":
			bad("%s: kind %s comes in a later phase of spec 0004", where, src.Kind)
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
