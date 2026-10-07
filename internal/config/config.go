// Package config loads kista's configuration: a YAML file plus KISTA_<PATH> environment overrides
// (spec 0003). Unknown keys and unknown KISTA_ variables are refused; settings that switch off a
// guard can only be set in the file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
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
	Signers  Signers  `yaml:"signers"`
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

// Signers configures where signing keys may come from.
type Signers struct {
	AllowFile bool   `yaml:"allow_file" kista:"fileonly"`
	FileDir   string `yaml:"file_dir" kista:"fileonly"`
}

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
	switch c.Azure.Identity.Kind {
	case "", "managed", "workload":
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
	if len(errs) > 0 {
		return fmt.Errorf("config: %s", strings.Join(errs, "; "))
	}
	return nil
}

// FileSignersAllowed reports whether file: signer references may be used.
func (c Config) FileSignersAllowed() bool {
	return c.Signers.FileDir != "" && (c.Profile == ProfileDev || c.Signers.AllowFile)
}
