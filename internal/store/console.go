package store

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// ConsoleClient is the administration console's public client (PKCE) at an issuer (spec 0015): a
// tenant issuer record's, or a server issuer's from config.
type ConsoleClient struct {
	IssuerID          string
	ClientID          string
	Scopes            []string
	AudienceParameter string // a parameter name some IdPs take the audience as; empty: none
	Audience          string // the audience the console's tokens carry; empty: the tenant's canonical one
	CreatedAt         time.Time
	CreatedBy         string
}

var (
	scopeToken = regexp.MustCompile(`^[\x21\x23-\x5b\x5d-\x7e]{1,128}$`) // RFC 6749 scope-token
	paramName  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)
)

// Check validates a console client's fields.
func (c ConsoleClient) Check() error {
	if n := len(c.ClientID); n == 0 || n > 200 || strings.ContainsFunc(c.ClientID, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return fmt.Errorf("%w: client_id is 1..200 printable characters", ErrInvalid)
	}
	if len(c.Scopes) > 20 || !slices.Contains(c.Scopes, "openid") {
		return fmt.Errorf("%w: scopes are at most 20 and include openid", ErrInvalid)
	}
	for _, s := range c.Scopes {
		if !scopeToken.MatchString(s) {
			return fmt.Errorf("%w: %q is not a scope", ErrInvalid, s)
		}
	}
	if len(strings.Join(c.Scopes, " ")) > 1000 {
		return fmt.Errorf("%w: the scopes are at most 1,000 characters together", ErrInvalid)
	}
	if c.AudienceParameter != "" && !paramName.MatchString(c.AudienceParameter) {
		return fmt.Errorf("%w: audience_parameter is a parameter name", ErrInvalid)
	}
	if len(c.Audience) > 400 {
		return fmt.Errorf("%w: the audience is too long", ErrInvalid)
	}
	return nil
}

// SetConsoleClient sets or replaces an issuer record's console client.
func (t *Tx) SetConsoleClient(ctx context.Context, c *ConsoleClient) error {
	if err := c.Check(); err != nil {
		return err
	}
	c.CreatedAt = t.Now()
	if _, err := t.exec(ctx, "DELETE FROM issuer_console_clients WHERE issuer_id = ?", c.IssuerID); err != nil {
		return err
	}
	_, err := t.exec(ctx, "INSERT INTO issuer_console_clients (issuer_id, client_id, scopes, audience_parameter, audience, created_at, created_by) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?)", c.IssuerID, c.ClientID, strings.Join(c.Scopes, " "), c.AudienceParameter, c.Audience,
		t.s.d.timeArg(c.CreatedAt), c.CreatedBy)
	return t.s.mapErr(err, "console client")
}

// RemoveConsoleClient removes an issuer record's console client; ErrNotFound if it has none.
func (t *Tx) RemoveConsoleClient(ctx context.Context, issuerID string) error {
	res, err := t.exec(ctx, "DELETE FROM issuer_console_clients WHERE issuer_id = ?", issuerID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err == nil {
			err = fmt.Errorf("%w: console client", ErrNotFound)
		}
		return err
	}
	return nil
}

// GetConsoleClient reads an issuer record's console client.
func (t *Tx) GetConsoleClient(ctx context.Context, issuerID string) (ConsoleClient, error) {
	c := ConsoleClient{IssuerID: issuerID}
	var scopes string
	err := t.queryRow(ctx, "SELECT client_id, scopes, audience_parameter, audience, created_at, created_by FROM issuer_console_clients WHERE issuer_id = ?",
		issuerID).Scan(&c.ClientID, &scopes, &c.AudienceParameter, &c.Audience, scanTime{&c.CreatedAt}, &c.CreatedBy)
	c.Scopes = strings.Fields(scopes)
	return c, notFound(err, "console client")
}

// ConsoleClientUses reports whether a console client of the tenant's issuers asks for an audience.
func (t *Tx) ConsoleClientUses(ctx context.Context, tenantID, audience string) (bool, error) {
	var n int
	err := t.queryRow(ctx, "SELECT COUNT(*) FROM issuer_console_clients c JOIN issuers i ON i.id = c.issuer_id WHERE i.tenant_id = ? AND c.audience = ?",
		tenantID, audience).Scan(&n)
	return n > 0, err
}

// ConsoleClients lists a tenant's console clients with their issuer records, by issuer name.
func (s *Store) ConsoleClients(ctx context.Context, tenantID string) ([]Issuer, []ConsoleClient, error) {
	var iss []Issuer
	var ccs []ConsoleClient
	// the clients' columns renamed in a derived table: issuerCols are unqualified
	q := "SELECT " + issuerCols + ", c.client_id, c.scopes, c.audience_parameter, c.audience, c.c_at, c.c_by FROM issuers " +
		"JOIN (SELECT issuer_id, client_id, scopes, audience_parameter, audience, created_at AS c_at, created_by AS c_by " +
		"FROM issuer_console_clients) c ON c.issuer_id = issuers.id WHERE tenant_id = ? ORDER BY name"
	err := s.eachRow(ctx, q, []any{tenantID}, func(sc func(...any) error) error {
		var c ConsoleClient
		var scopes string
		is, err := scanIssuer(scanFunc(func(dest ...any) error {
			return sc(append(dest, &c.ClientID, &scopes, &c.AudienceParameter, &c.Audience, scanTime{&c.CreatedAt}, &c.CreatedBy)...)
		}))
		if err != nil {
			return err
		}
		c.IssuerID, c.Scopes = is.ID, strings.Fields(scopes)
		iss, ccs = append(iss, is), append(ccs, c)
		return nil
	})
	return iss, ccs, err
}

type scanFunc func(dest ...any) error

func (f scanFunc) Scan(dest ...any) error { return f(dest...) }
