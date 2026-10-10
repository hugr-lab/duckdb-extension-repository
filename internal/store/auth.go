package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Principal kinds (spec 0006).
const (
	PrincipalSubject = "subject"
	PrincipalRole    = "role"
	PrincipalGroup   = "group"
	PrincipalClient  = "client"
	PrincipalIssuer  = "issuer"
	// PrincipalPublisher is a publisher of spec 0008: it publishes and promotes only.
	PrincipalPublisher = "publisher"
)

// Grant verbs: spec 0006's, and spec 0008's publish and promote (never implied by admin).
const (
	VerbInstall = "install"
	VerbAdmin   = "admin"
	VerbPublish = "publish"
	VerbPromote = "promote"
	VerbAudit   = "audit" // read a tenant's events and statistics (spec 0010)
)

// Verbs are every grant verb.
var Verbs = []string{VerbInstall, VerbAdmin, VerbPublish, VerbPromote, VerbAudit}

// MaxGrants is the most grants a tenant may hold: every request's authorization is linear in them.
const MaxGrants = 1000

// MaxIssuers is the most issuer records a tenant may hold.
const MaxIssuers = 16

// Issuer is a tenant's issuer record.
type Issuer struct {
	ID, TenantID, Name, URL, JWKSURI string
	Algorithms                       []string
	RequiredClaims                   map[string]string
	RolesClaim, GroupsClaim          []string // claim paths: lists of keys
	ClientClaim                      []string
	MaxTokenLifetime                 time.Duration
	CreatedAt                        time.Time
	CreatedBy                        string
}

var issuerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// ValidIssuerName checks an issuer record's name.
func ValidIssuerName(n string) error {
	if !issuerName.MatchString(n) {
		return fmt.Errorf("%w: issuer name %q must match [a-z][a-z0-9-]{0,15}", ErrInvalid, n)
	}
	return nil
}

const issuerCols = `id, tenant_id, name, url, jwks_uri, algorithms, required_claims, roles_claim, groups_claim,
client_claim, max_token_lifetime, created_at, created_by`

func scanIssuer(r interface{ Scan(...any) error }) (Issuer, error) {
	var is Issuer
	var jwks sql.NullString
	var algs, req, roles, groups, client string
	var life int64
	err := r.Scan(&is.ID, &is.TenantID, &is.Name, &is.URL, &jwks, &algs, &req, &roles, &groups, &client, &life,
		scanTime{&is.CreatedAt}, &is.CreatedBy)
	if err != nil {
		return is, err
	}
	is.JWKSURI, is.Algorithms, is.MaxTokenLifetime = jwks.String, strings.Fields(algs), time.Duration(life)*time.Second
	for _, x := range []struct {
		src string
		dst any
	}{{req, &is.RequiredClaims}, {roles, &is.RolesClaim}, {groups, &is.GroupsClaim}, {client, &is.ClientClaim}} {
		if err := json.Unmarshal([]byte(x.src), x.dst); err != nil {
			return is, fmt.Errorf("store: issuer %s: %w", is.Name, err)
		}
	}
	return is, nil
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// InsertIssuer adds an issuer record to a tenant (at most MaxIssuers).
func (t *Tx) InsertIssuer(ctx context.Context, is *Issuer) error {
	if err := ValidIssuerName(is.Name); err != nil {
		return err
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM issuers WHERE tenant_id = ?", is.TenantID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxIssuers {
		return fmt.Errorf("%w: a tenant holds at most %d issuer records", ErrInvalid, MaxIssuers)
	}
	if is.RequiredClaims == nil {
		is.RequiredClaims = map[string]string{}
	}
	req := jsonOf(is.RequiredClaims)
	roles, groups, client := jsonOf(nonNil(is.RolesClaim)), jsonOf(nonNil(is.GroupsClaim)), jsonOf(nonNil(is.ClientClaim))
	for _, p := range [][]string{is.RolesClaim, is.GroupsClaim, is.ClientClaim} {
		for _, k := range p {
			if k == "" {
				return fmt.Errorf("%w: a claim path has an empty key", ErrInvalid)
			}
		}
	}
	switch {
	case len(req) > 4000:
		return fmt.Errorf("%w: required claims are too long", ErrInvalid)
	case len(roles) > 400 || len(groups) > 400 || len(client) > 400:
		return fmt.Errorf("%w: a claim path is too long", ErrInvalid)
	case len(strings.Join(is.Algorithms, " ")) > 200 || len(is.URL) > 512 || len(is.JWKSURI) > 512 || len(is.CreatedBy) > 400:
		return fmt.Errorf("%w: a value is too long", ErrInvalid)
	}
	is.ID, is.CreatedAt = NewID(), t.Now()
	_, err := t.exec(ctx, "INSERT INTO issuers ("+issuerCols+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		is.ID, is.TenantID, is.Name, is.URL, nullable(is.JWKSURI), strings.Join(is.Algorithms, " "), req,
		roles, groups, client,
		int64(is.MaxTokenLifetime/time.Second), t.s.d.timeArg(is.CreatedAt), is.CreatedBy)
	if err := t.s.mapErr(err, "issuer "+is.Name+" (or its URL)"); err != nil {
		return err
	}
	return t.BumpAuth(ctx, is.TenantID)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// GetIssuer reads a tenant's issuer record by name.
func (t *Tx) GetIssuer(ctx context.Context, tenantID, name string) (Issuer, error) {
	is, err := scanIssuer(t.queryRow(ctx, "SELECT "+issuerCols+" FROM issuers WHERE tenant_id = ? AND name = ?", tenantID, name))
	return is, notFound(err, "issuer "+name)
}

// DeleteIssuer removes an issuer record and its grants: a record added later under the same name
// starts with none.
func (t *Tx) DeleteIssuer(ctx context.Context, tenantID, name string) error {
	var id string
	if err := t.queryRow(ctx, "SELECT id FROM issuers WHERE tenant_id = ? AND name = ?", tenantID, name).Scan(&id); err != nil {
		return notFound(err, "issuer "+name)
	}
	if _, err := t.exec(ctx, "DELETE FROM grants WHERE issuer_id = ?", id); err != nil {
		return err
	}
	if _, err := t.exec(ctx, "DELETE FROM issuer_console_clients WHERE issuer_id = ?", id); err != nil {
		return err
	}
	if _, err := t.exec(ctx, "DELETE FROM issuers WHERE id = ?", id); err != nil {
		return err
	}
	return t.BumpAuth(ctx, tenantID)
}

// BumpAuth increments a tenant's auth_version: caches of issuers, audiences and grants see it.
func (t *Tx) BumpAuth(ctx context.Context, tenantID string) error {
	_, err := t.exec(ctx, "UPDATE tenants SET auth_version = auth_version + 1 WHERE id = ?", tenantID)
	return err
}

// --- audiences ---

// AddAudience assigns an audience to a tenant; audiences are unique across tenants.
func (t *Tx) AddAudience(ctx context.Context, tenantID, aud, actor string) error {
	if aud == "" || len(aud) > 400 || strings.ContainsAny(aud, " \t\r\n") {
		return fmt.Errorf("%w: audience %q", ErrInvalid, aud)
	}
	_, err := t.exec(ctx, "INSERT INTO tenant_audiences (audience, tenant_id, created_at, created_by) VALUES (?, ?, ?, ?)",
		aud, tenantID, t.s.d.timeArg(t.Now()), actor)
	if err := t.s.mapErr(err, "audience "+aud); err != nil {
		return err
	}
	return t.BumpAuth(ctx, tenantID)
}

// TenantAudiences lists a tenant's assigned audiences.
func (t *Tx) TenantAudiences(ctx context.Context, tenantID string) ([]string, error) {
	rows, err := t.query(ctx, "SELECT audience FROM tenant_audiences WHERE tenant_id = ? ORDER BY audience", tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RemoveAudience removes a tenant's audience.
func (t *Tx) RemoveAudience(ctx context.Context, tenantID, aud string) error {
	res, err := t.exec(ctx, "DELETE FROM tenant_audiences WHERE tenant_id = ? AND audience = ?", tenantID, aud)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: audience %s", ErrNotFound, aud)
	}
	return t.BumpAuth(ctx, tenantID)
}

// --- grants ---

// Grant is principal × resource × verbs in a tenant.
type Grant struct {
	ID, TenantID, IssuerID, Kind, Value string
	ChannelID, Extension                string // empty: every channel / every extension
	Verbs                               []string
	CreatedAt                           time.Time
	CreatedBy                           string
	IssuerName, ChannelName             string // filled by reads
	// PublisherID is a publisher grant's publisher (spec 0008): its Kind is publisher, its Value the
	// publisher's id, and it has no issuer.
	PublisherID, PublisherName string
}

// Principal renders the grant's principal: kind:<issuer>|value, issuer:<issuer>, or
// publisher:<name>.
func (g Grant) Principal() string {
	switch g.Kind {
	case PrincipalIssuer:
		return PrincipalIssuer + ":" + g.IssuerName
	case PrincipalPublisher:
		return PrincipalPublisher + ":" + g.PublisherName
	}
	return g.Kind + ":" + g.IssuerName + "|" + g.Value
}

// InsertGrant adds a grant.
func (t *Tx) InsertGrant(ctx context.Context, g *Grant) error {
	if !slices.Contains([]string{PrincipalSubject, PrincipalRole, PrincipalGroup, PrincipalClient, PrincipalIssuer, PrincipalPublisher}, g.Kind) {
		return fmt.Errorf("%w: principal kind %q", ErrInvalid, g.Kind)
	}
	if g.Kind == PrincipalPublisher {
		// a publisher only publishes and promotes (spec 0008)
		if g.PublisherID == "" || g.IssuerID != "" {
			return fmt.Errorf("%w: a publisher grant names a publisher", ErrInvalid)
		}
		g.Value = g.PublisherID
		for _, v := range g.Verbs {
			if v != VerbPublish && v != VerbPromote {
				return fmt.Errorf("%w: a publisher may only be granted publish and promote", ErrInvalid)
			}
		}
	} else if g.PublisherID != "" {
		return fmt.Errorf("%w: only a publisher grant names a publisher", ErrInvalid)
	}
	if (g.Kind == PrincipalIssuer) != (g.Value == "") || len(g.Value) > 256 {
		return fmt.Errorf("%w: principal value", ErrInvalid)
	}
	if len(g.Verbs) == 0 {
		return fmt.Errorf("%w: a grant needs a verb", ErrInvalid)
	}
	for _, v := range g.Verbs {
		if !slices.Contains(Verbs, v) {
			return fmt.Errorf("%w: verb %q (install, admin, publish, promote or audit)", ErrInvalid, v)
		}
		// audit reads the tenant's log: a tenant-wide grant, never an issuer-wide one (spec 0010)
		if v == VerbAudit && (g.ChannelID != "" || g.Extension != "" || g.Kind == PrincipalIssuer) {
			return fmt.Errorf("%w: audit is granted on the tenant, never issuer-wide", ErrInvalid)
		}
	}
	if len(g.CreatedBy) > 400 {
		return fmt.Errorf("%w: a value is too long", ErrInvalid)
	}
	var total int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM grants WHERE tenant_id = ?", g.TenantID).Scan(&total); err != nil {
		return err
	}
	if total >= MaxGrants {
		return fmt.Errorf("%w: a tenant holds at most %d grants", ErrInvalid, MaxGrants)
	}
	slices.Sort(g.Verbs)
	var n int
	if err := t.queryRow(ctx, `SELECT COUNT(*) FROM grants WHERE tenant_id = ? AND COALESCE(issuer_id, '') = ?
AND COALESCE(publisher_id, '') = ? AND kind = ? AND value = ? AND COALESCE(channel_id, '') = ? AND COALESCE(extension, '') = ?
AND verbs = ?`, g.TenantID, g.IssuerID, g.PublisherID, g.Kind, g.Value,
		g.ChannelID, g.Extension, strings.Join(g.Verbs, " ")).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: the same grant", ErrExists)
	}
	g.ID, g.CreatedAt = NewID(), t.Now()
	_, err := t.exec(ctx, `INSERT INTO grants (id, tenant_id, issuer_id, publisher_id, kind, value, channel_id, extension, verbs,
created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, g.ID, g.TenantID, nullable(g.IssuerID), nullable(g.PublisherID),
		g.Kind, g.Value, nullable(g.ChannelID), nullable(g.Extension), strings.Join(g.Verbs, " "), t.s.d.timeArg(g.CreatedAt), g.CreatedBy)
	if err := t.s.mapErr(err, "grant"); err != nil {
		return err
	}
	return t.BumpAuth(ctx, g.TenantID)
}

// DeleteGrant removes a grant.
func (t *Tx) DeleteGrant(ctx context.Context, tenantID, id string) error {
	res, err := t.exec(ctx, "DELETE FROM grants WHERE tenant_id = ? AND id = ?", tenantID, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: grant %s", ErrNotFound, id)
	}
	return t.BumpAuth(ctx, tenantID)
}

// TenantAuth is everything token verification and grant checks read for a tenant.
type TenantAuth struct {
	Issuers    []Issuer
	Audiences  []string
	Grants     []Grant
	Publishers []Publisher // with their GitHub credentials (spec 0008)
}

// GetTenantAuth reads a tenant's issuers, assigned audiences and grants.
func (s *Store) GetTenantAuth(ctx context.Context, tenantID string) (TenantAuth, error) {
	var a TenantAuth
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT "+issuerCols+" FROM issuers WHERE tenant_id = ? ORDER BY name"), tenantID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		is, err := scanIssuer(rows)
		if err != nil {
			rows.Close()
			return a, err
		}
		a.Issuers = append(a.Issuers, is)
	}
	if err := closeRows(rows); err != nil {
		return a, err
	}
	rows, err = s.db.QueryContext(ctx, s.d.rebind("SELECT audience FROM tenant_audiences WHERE tenant_id = ? ORDER BY audience"), tenantID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var aud string
		if err := rows.Scan(&aud); err != nil {
			rows.Close()
			return a, err
		}
		a.Audiences = append(a.Audiences, aud)
	}
	if err := closeRows(rows); err != nil {
		return a, err
	}
	rows, err = s.db.QueryContext(ctx, s.d.rebind(`SELECT g.id, g.tenant_id, g.issuer_id, g.publisher_id, g.kind, g.value, g.channel_id,
g.extension, g.verbs, g.created_at, g.created_by, i.name, p.name, c.name FROM grants g LEFT JOIN issuers i ON i.id = g.issuer_id
LEFT JOIN publishers p ON p.id = g.publisher_id LEFT JOIN channels c ON c.id = g.channel_id WHERE g.tenant_id = ?
ORDER BY g.created_at, g.id`), tenantID)
	if err != nil {
		return a, err
	}
	for rows.Next() {
		var g Grant
		var iss, pub, ch, ext, issName, pubName, chName sql.NullString
		var verbs string
		if err := rows.Scan(&g.ID, &g.TenantID, &iss, &pub, &g.Kind, &g.Value, &ch, &ext, &verbs, scanTime{&g.CreatedAt},
			&g.CreatedBy, &issName, &pubName, &chName); err != nil {
			rows.Close()
			return a, err
		}
		g.IssuerID, g.PublisherID, g.IssuerName, g.PublisherName = iss.String, pub.String, issName.String, pubName.String
		g.ChannelID, g.Extension, g.ChannelName, g.Verbs = ch.String, ext.String, chName.String, strings.Fields(verbs)
		a.Grants = append(a.Grants, g)
	}
	if err := closeRows(rows); err != nil {
		return a, err
	}
	a.Publishers, err = s.publishers(ctx, tenantID)
	return a, err
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	rows.Close()
	return err
}

// TenantAdminGrants counts a tenant's tenant-wide grants that carry admin and are evaluated (not
// issuer-wide ones, spec 0007): the lock-out check counts them inside the removing transaction.
func (t *Tx) TenantAdminGrants(ctx context.Context, tenantID string) (int, error) {
	rows, err := t.query(ctx, `SELECT verbs FROM grants WHERE tenant_id = ? AND channel_id IS NULL AND extension IS NULL
AND kind <> ?`, tenantID, PrincipalIssuer)
	if err != nil {
		return 0, err
	}
	n := 0
	for rows.Next() {
		var verbs string
		if err := rows.Scan(&verbs); err != nil {
			rows.Close()
			return 0, err
		}
		if slices.Contains(strings.Fields(verbs), VerbAdmin) {
			n++
		}
	}
	return n, closeRows(rows)
}

// AssignedAudiences lists every tenant's assigned audiences: kista serve refuses to start when one
// is a server audience (spec 0007).
func (s *Store) AssignedAudiences(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT audience FROM tenant_audiences ORDER BY audience")
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	return out, closeRows(rows)
}
