package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Publisher is an identity of a tenant that only publishes and promotes (spec 0008), authenticated
// by its credentials.
type Publisher struct {
	ID, TenantID, Name string
	CreatedAt          time.Time
	CreatedBy          string
	GitHub             []GitHubCredential
}

// GitHubCredential binds a publisher to GitHub Actions runs: immutable owner and repository ids, a
// workflow, and optionally a ref pattern and an environment.
type GitHubCredential struct {
	ID, PublisherID, Provider       string
	OwnerID, RepositoryID, Workflow string
	Ref, Environment                string // "": any
	CreatedAt                       time.Time
	CreatedBy                       string
}

// MaxPublishers is the most publishers a tenant may hold, and MaxCredentials the most credentials
// one publisher may hold.
const (
	MaxPublishers  = 200
	MaxCredentials = 20
)

var (
	publisherName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	digits        = regexp.MustCompile(`^[0-9]{1,20}$`)
)

// InsertPublisher adds a publisher.
func (t *Tx) InsertPublisher(ctx context.Context, p *Publisher) error {
	if !publisherName.MatchString(p.Name) {
		return fmt.Errorf("%w: publisher name %q must match [a-z][a-z0-9-]{0,63}", ErrInvalid, p.Name)
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM publishers WHERE tenant_id = ?", p.TenantID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxPublishers {
		return fmt.Errorf("%w: a tenant holds at most %d publishers", ErrInvalid, MaxPublishers)
	}
	p.ID, p.CreatedAt = NewID(), t.Now()
	_, err := t.exec(ctx, "INSERT INTO publishers (id, tenant_id, name, created_at, created_by) VALUES (?, ?, ?, ?, ?)",
		p.ID, p.TenantID, p.Name, t.s.d.timeArg(p.CreatedAt), p.CreatedBy)
	if err := t.s.mapErr(err, "publisher "+p.Name); err != nil {
		return err
	}
	return t.BumpAuth(ctx, p.TenantID)
}

// GetPublisher reads a tenant's publisher by name.
func (t *Tx) GetPublisher(ctx context.Context, tenantID, name string) (Publisher, error) {
	var p Publisher
	err := t.queryRow(ctx, "SELECT id, tenant_id, name, created_at, created_by FROM publishers WHERE tenant_id = ? AND name = ?",
		tenantID, name).Scan(&p.ID, &p.TenantID, &p.Name, scanTime{&p.CreatedAt}, &p.CreatedBy)
	return p, notFound(err, "publisher "+name)
}

// DeletePublisher removes a publisher, its credentials and its grants.
func (t *Tx) DeletePublisher(ctx context.Context, tenantID, name string) error {
	p, err := t.GetPublisher(ctx, tenantID, name)
	if err != nil {
		return err
	}
	for _, q := range []string{"DELETE FROM grants WHERE publisher_id = ?", "DELETE FROM publisher_github WHERE publisher_id = ?",
		"DELETE FROM api_keys WHERE publisher_id = ?", "DELETE FROM publishers WHERE id = ?"} {
		if _, err := t.exec(ctx, q, p.ID); err != nil {
			return err
		}
	}
	return t.BumpAuth(ctx, tenantID)
}

// InsertGitHubCredential adds a GitHub credential to a publisher.
func (t *Tx) InsertGitHubCredential(ctx context.Context, tenantID string, c *GitHubCredential) error {
	switch {
	case !digits.MatchString(c.OwnerID) || !digits.MatchString(c.RepositoryID):
		return fmt.Errorf("%w: owner_id and repository_id are GitHub's numeric ids", ErrInvalid)
	case c.Workflow == "" || len(c.Workflow) > 400 || strings.Contains(c.Workflow, "@") || strings.Count(c.Workflow, "/") < 2:
		return fmt.Errorf("%w: workflow is <owner>/<repository>/<path> of the workflow file, without @<ref>", ErrInvalid)
	case len(c.Ref) > 256 || len(c.Environment) > 256 || c.Provider == "" || len(c.Provider) > 64:
		return fmt.Errorf("%w: a value is too long", ErrInvalid)
	}
	var n, same int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM publisher_github WHERE publisher_id = ?", c.PublisherID).Scan(&n); err != nil {
		return err
	}
	if err := t.queryRow(ctx, `SELECT COUNT(*) FROM publisher_github WHERE publisher_id = ? AND provider = ? AND owner_id = ?
AND repository_id = ? AND workflow = ? AND COALESCE(ref, '') = ? AND COALESCE(environment, '') = ?`, c.PublisherID, c.Provider,
		c.OwnerID, c.RepositoryID, c.Workflow, c.Ref, c.Environment).Scan(&same); err != nil {
		return err
	}
	if same > 0 {
		return fmt.Errorf("%w: the same credential", ErrExists)
	}
	if n >= MaxCredentials {
		return fmt.Errorf("%w: a publisher holds at most %d credentials", ErrInvalid, MaxCredentials)
	}
	c.ID, c.CreatedAt = NewID(), t.Now()
	_, err := t.exec(ctx, `INSERT INTO publisher_github (id, publisher_id, provider, owner_id, repository_id, workflow, ref, environment,
created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, c.PublisherID, c.Provider, c.OwnerID, c.RepositoryID,
		c.Workflow, nullable(c.Ref), nullable(c.Environment), t.s.d.timeArg(c.CreatedAt), c.CreatedBy)
	if err := t.s.mapErr(err, "credential"); err != nil {
		return err
	}
	return t.BumpAuth(ctx, tenantID)
}

// DeleteGitHubCredential removes a credential of a tenant's publisher.
func (t *Tx) DeleteGitHubCredential(ctx context.Context, tenantID, publisherID, id string) error {
	res, err := t.exec(ctx, "DELETE FROM publisher_github WHERE id = ? AND publisher_id = ?", id, publisherID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: credential %s", ErrNotFound, id)
	}
	return t.BumpAuth(ctx, tenantID)
}

// publishers reads a tenant's publishers with their credentials, by name.
func (s *Store) publishers(ctx context.Context, tenantID string) ([]Publisher, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind(`SELECT p.id, p.tenant_id, p.name, p.created_at, p.created_by, g.id, g.provider,
g.owner_id, g.repository_id, g.workflow, g.ref, g.environment, g.created_at, g.created_by
FROM publishers p LEFT JOIN publisher_github g ON g.publisher_id = p.id WHERE p.tenant_id = ? ORDER BY p.name, g.created_at, g.id`), tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Publisher
	for rows.Next() {
		var p Publisher
		var gid, prov, owner, repo, wf, ref, env, gby sql.NullString
		var gt time.Time // scanTime takes NULL as the zero time
		if err := rows.Scan(&p.ID, &p.TenantID, &p.Name, scanTime{&p.CreatedAt}, &p.CreatedBy, &gid, &prov, &owner, &repo, &wf, &ref,
			&env, scanTime{&gt}, &gby); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != p.ID {
			out = append(out, p)
		}
		if gid.Valid {
			last := &out[len(out)-1]
			last.GitHub = append(last.GitHub, GitHubCredential{ID: gid.String, PublisherID: p.ID, Provider: prov.String,
				OwnerID: owner.String, RepositoryID: repo.String, Workflow: wf.String, Ref: ref.String, Environment: env.String,
				CreatedAt: gt, CreatedBy: gby.String})
		}
	}
	return out, rows.Err()
}

// IssuerURLs lists every tenant's issuer records as (tenant id, name, URL): kista serve warns about
// records at a trusted-publishing provider's URL, which are never used (spec 0008).
func (s *Store) IssuerURLs(ctx context.Context) ([][3]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT tenant_id, name, url FROM issuers ORDER BY tenant_id, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][3]string
	for rows.Next() {
		var r [3]string
		if err := rows.Scan(&r[0], &r[1], &r[2]); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// APIKey is a publisher's API key (spec 0008 phase 2): only its SHA-256 is stored.
type APIKey struct {
	ID, PublisherID, Prefix, Hash string
	ExpiresAt, CreatedAt          time.Time
	CreatedBy                     string
	LastUsedAt                    time.Time // zero: never used
}

// MaxAPIKeys is the most keys one publisher may hold.
const MaxAPIKeys = 10

// InsertAPIKey adds a key (its hash) to a publisher, which holds at most MaxAPIKeys unexpired keys
// (expired ones are removed here). Callers hold the tenant's auth lock, so the cap holds.
func (t *Tx) InsertAPIKey(ctx context.Context, k *APIKey) error {
	if !hexHash.MatchString(k.Hash) || len(k.Prefix) == 0 || len(k.Prefix) > 16 {
		return fmt.Errorf("%w: a key", ErrInvalid)
	}
	if _, err := t.exec(ctx, "DELETE FROM api_keys WHERE publisher_id = ? AND expires_at <= ?", k.PublisherID, t.s.d.timeArg(t.Now())); err != nil {
		return err
	}
	var n int
	if err := t.queryRow(ctx, "SELECT COUNT(*) FROM api_keys WHERE publisher_id = ?", k.PublisherID).Scan(&n); err != nil {
		return err
	}
	if n >= MaxAPIKeys {
		return fmt.Errorf("%w: a publisher holds at most %d keys", ErrInvalid, MaxAPIKeys)
	}
	k.ID, k.CreatedAt = NewID(), t.Now()
	_, err := t.exec(ctx, `INSERT INTO api_keys (id, publisher_id, prefix, hash, expires_at, created_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?)`, k.ID, k.PublisherID, k.Prefix, k.Hash, t.s.d.timeArg(k.ExpiresAt), t.s.d.timeArg(k.CreatedAt), k.CreatedBy)
	return t.s.mapErr(err, "key")
}

// DeleteAPIKey removes a publisher's key. Keys are looked up on every request, never cached, so no
// cache is bumped.
func (t *Tx) DeleteAPIKey(ctx context.Context, publisherID, id string) error {
	res, err := t.exec(ctx, "DELETE FROM api_keys WHERE id = ? AND publisher_id = ?", id, publisherID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: key %s", ErrNotFound, id)
	}
	return nil
}

const apiKeyCols = "id, publisher_id, prefix, hash, expires_at, created_at, created_by, last_used_at"

func scanAPIKey(r interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	err := r.Scan(&k.ID, &k.PublisherID, &k.Prefix, &k.Hash, scanTime{&k.ExpiresAt}, scanTime{&k.CreatedAt}, &k.CreatedBy,
		scanTime{&k.LastUsedAt})
	return k, err
}

// APIKeys lists a publisher's keys, oldest first.
func (s *Store) APIKeys(ctx context.Context, publisherID string) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, s.d.rebind("SELECT "+apiKeyCols+" FROM api_keys WHERE publisher_id = ? ORDER BY created_at, id"), publisherID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// APIKeyByHash finds a key by its hash, with its publisher (ErrNotFound for none).
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (APIKey, Publisher, error) {
	var p Publisher
	row := s.db.QueryRowContext(ctx, s.d.rebind(`SELECT k.id, k.publisher_id, k.prefix, k.hash, k.expires_at, k.created_at, k.created_by,
k.last_used_at, p.tenant_id, p.name, p.created_at, p.created_by FROM api_keys k JOIN publishers p ON p.id = k.publisher_id
WHERE k.hash = ?`), hash)
	var k APIKey
	err := row.Scan(&k.ID, &k.PublisherID, &k.Prefix, &k.Hash, scanTime{&k.ExpiresAt}, scanTime{&k.CreatedAt}, &k.CreatedBy,
		scanTime{&k.LastUsedAt}, &p.TenantID, &p.Name, scanTime{&p.CreatedAt}, &p.CreatedBy)
	p.ID = k.PublisherID
	return k, p, notFound(err, "key")
}

// TouchAPIKey records a key's use (callers do it at most once an hour).
func (s *Store) TouchAPIKey(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.d.rebind("UPDATE api_keys SET last_used_at = ? WHERE id = ?"), s.d.timeArg(s.Now()), id)
	return err
}
