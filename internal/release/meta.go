package release

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/icon"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0036 phase 1: a catalogue entry's metadata, an icon and a summary. A tenant's administrators
// write the tenant's own sources' entries; a distribution admin or a server administrator a
// distributor's publisher's; a publisher, by a signed request, its own. Every refusal is the same
// not-found.

// MaxSummary is a summary's length in code points, after NFC.
const MaxSummary = 160

// maxMarks is how many combining marks may follow a base character.
const maxMarks = 4

var (
	// ErrStale is a signed metadata write older than the entry's last one, or of the same second
	// with another body: a captured request never undoes a newer write (409 stale).
	ErrStale = errors.New("release: the signed request is older than the entry's last metadata write; read it again")
	// ErrIconChanged is a write keeping an icon by a hash that is no longer the stored one: someone
	// else wrote meanwhile (409 conflict).
	ErrIconChanged = errors.New("release: the icon changed; read the metadata again")
)

// sha256Hex is a stored icon's hash, as a write keeping it names it.
var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// fillers are printable characters that show nothing: Enterest's summary rules refuse them too.
var fillers = map[rune]bool{0x115F: true, 0x1160: true, 0x3164: true, 0xFFA0: true, 0x2800: true}

// CheckSummary checks a summary (spec 0036) and returns it as stored: trimmed of spaces and
// normalised to NFC, "" for none. It is plain text: at most 160 code points, each printable or a
// space, no format, line or paragraph separator, no invisible filler, and at most 4 combining marks
// after any base character.
func CheckSummary(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: the summary is not valid UTF-8", store.ErrInvalid)
	}
	s = norm.NFC.String(strings.TrimSpace(s))
	if n := utf8.RuneCountInString(s); n > MaxSummary {
		return "", fmt.Errorf("%w: the summary is at most %d characters (this one has %d)", store.ErrInvalid, MaxSummary, n)
	}
	marks := 0
	for _, r := range s {
		switch {
		// IsPrint already excludes Cf, Zl and Zp (and every space but U+0020); they are named for the reader
		case unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) || !unicode.IsPrint(r) && r != ' ':
			return "", fmt.Errorf("%w: the summary is plain text on one line: it has a control, format or separator character (U+%04X)",
				store.ErrInvalid, r)
		case fillers[r]:
			return "", fmt.Errorf("%w: the summary has an invisible filler (U+%04X)", store.ErrInvalid, r)
		case unicode.Is(unicode.M, r):
			if marks++; marks > maxMarks {
				return "", fmt.Errorf("%w: the summary has more than %d combining marks after a character", store.ErrInvalid, maxMarks)
			}
		default:
			marks = 0
		}
	}
	return s, nil
}

// IconInput is a write's icon: a new one's bytes and declared type, or the stored one kept by its
// hash.
type IconInput struct {
	ContentType string
	Data        []byte
	Keep        string // the stored icon's hash
}

// MetaWrite is a metadata write: the whole metadata, replacing what the entry had (a field nil
// clears it). A signed request's carry its signed-at, its body's hash and the signing key.
type MetaWrite struct {
	Summary *string
	Icon    *IconInput
	// SignedAt, BodySHA256, KeyID and KeyFingerprint are a signed request's (zero for a token's)
	SignedAt                          time.Time
	BodySHA256, KeyID, KeyFingerprint string
}

// EntryMetaView is an entry's metadata as answers show it: the row (its fields zero for an entry
// without metadata), the source's label, and the distributor of a distributor's publisher's entry.
type EntryMetaView struct {
	store.EntryMeta
	Label, Distributor string
}

// metaTarget is an entry whose metadata a caller reaches.
type metaTarget struct {
	t                         store.Tenant
	typ, src, label, name, dt string // dt: the source's distributor, "" for the tenant's own
}

// metaTarget decides whether a caller reaches an entry's metadata (spec 0036), to write or to read,
// and resolves it: a tenant's own source needs a tenant-wide administrator; a distributor's
// publisher's source (live or removed) a distribution admin or a server administrator to write, and
// a catalogue reader (the tenant's administrators too) to read; a publisher, by a signed request,
// its own source only (publisher:<its name> or publisher-id:<its id>), a distributor's publisher a
// name it may publish. The entry must exist: a build of the name and type from the source. Every
// refusal is authz.ErrDenied or store.ErrNotFound, both the same 404.
func (s *Service) metaTarget(ctx context.Context, a authz.Actor, tenant, typ, label, name string, write bool) (metaTarget, error) {
	if typ == "" {
		typ = pkgtype.Default
	}
	if !pkgtype.Known(typ) {
		return metaTarget{}, fmt.Errorf("%w: package type %q", store.ErrInvalid, typ)
	}
	x := metaTarget{typ: typ, name: name}
	var err error
	var labels map[string]string // the tenant's sources as answers name them, read once
	switch {
	case a.Kind == authz.ActorPublisher:
		if a.Tenant != tenant {
			return x, authz.ErrDenied
		}
		if x.t, err = s.Store.GetTenant(ctx, tenant); err != nil {
			return x, err
		}
		if x.t.State != store.TenantActive {
			return x, authz.ErrDenied
		}
		x.src = sourceOf(a)
		id, ok := strings.CutPrefix(x.src, store.SourcePublisherPrefix)
		if !ok {
			return x, authz.ErrDenied
		}
		if labels, err = s.Store.SourceLabels(ctx, x.t.ID); err != nil {
			return x, err
		}
		if own, live := labels[x.src]; !live || label != own && label != store.SourcePublisherIDPrefix+id {
			return x, authz.ErrDenied // another source: refused (the same 404), and audited
		}
	case a.Kind == authz.ActorDistributionAdmin:
		if x.t, err = s.mayDistribute(ctx, a, tenant); err != nil {
			return x, err
		}
		x.src, err = s.Store.ResolveDistributorSource(ctx, x.t.ID, label)
	case serverActor(a):
		if x.t, err = s.mayAdminTenant(ctx, a, tenant); err != nil {
			return x, err
		}
		// a distributor's publisher (removed or not) as a distribution admin names it, else any source
		if x.src, err = s.Store.ResolveDistributorSource(ctx, x.t.ID, label); errors.Is(err, store.ErrNoSource) {
			x.src, err = s.Store.ResolveSource(ctx, x.t.ID, label)
		}
	case a.Kind == authz.ActorPrincipal:
		if x.t, err = s.mayAdminTenant(ctx, a, tenant); err != nil {
			return x, err
		}
		x.src, err = s.Store.ResolveSource(ctx, x.t.ID, label)
	default:
		return x, authz.ErrDenied
	}
	if errors.Is(err, store.ErrNoSource) {
		return x, fmt.Errorf("%w: no source %q in the tenant", store.ErrNotFound, label)
	}
	if err != nil {
		return x, err
	}
	if x.dt, err = s.Store.SourceDistributor(ctx, x.t.ID, x.src); err != nil {
		return x, err
	}
	if write && x.dt != "" && a.Kind == authz.ActorPrincipal {
		return x, authz.ErrDenied // a tenant's administrators never write a distributor's entry
	}
	if ok, err := s.Store.EntryExists(ctx, x.t.ID, typ, x.src, name); err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("%w: no build of %s from %s", store.ErrNotFound, name, label)
		}
		return x, err
	}
	if a.Kind == authz.ActorPublisher && x.dt != "" {
		if err := s.distributorMay(ctx, tenant, typ, name, x.dt); err != nil {
			return x, err
		}
	}
	// the source as answers name it
	if x.dt != "" {
		srcs, err := s.Store.DistributorSources(ctx, x.t.ID)
		if err != nil {
			return x, err
		}
		x.label = srcs[x.src].Label
	} else {
		if labels == nil {
			if labels, err = s.Store.SourceLabels(ctx, x.t.ID); err != nil {
				return x, err
			}
		}
		x.label = store.Label(labels, x.src)
	}
	return x, nil
}

func (x metaTarget) view(m store.EntryMeta, ok bool) EntryMetaView {
	if !ok {
		m = store.EntryMeta{TenantID: x.t.ID, PackageType: x.typ, Source: x.src, Name: x.name}
	}
	return EntryMetaView{EntryMeta: m, Label: x.label, Distributor: x.dt}
}

// EntryMeta reads an entry's metadata (spec 0036): the callers who may write it, and the catalogue's
// readers.
func (s *Service) EntryMeta(ctx context.Context, a authz.Actor, tenant, packageType, source, name string) (EntryMetaView, error) {
	x, err := s.metaTarget(ctx, a, tenant, packageType, source, name, false)
	if err != nil {
		return EntryMetaView{}, err
	}
	m, ok, err := s.Store.EntryMeta(ctx, x.t.ID, x.typ, x.src, name)
	return x.view(m, ok), err
}

// EntryIcon reads an entry's icon, checked against its row (blob.ErrCorrupt for an object missing
// or not matching): the metadata's readers. Nothing is read from the storage domain before the
// caller is authorized and the entry found.
func (s *Service) EntryIcon(ctx context.Context, a authz.Actor, tenant, packageType, source, name string) ([]byte, error) {
	x, err := s.metaTarget(ctx, a, tenant, packageType, source, name, false)
	if err != nil {
		return nil, err
	}
	m, ok, err := s.Store.EntryMeta(ctx, x.t.ID, x.typ, x.src, name)
	if err != nil {
		return nil, err
	}
	if !ok || m.IconSHA256 == "" {
		return nil, fmt.Errorf("%w: the entry has no icon", store.ErrNotFound)
	}
	return s.Blob.ReadIcon(ctx, x.t.StorageDomain, m.IconSHA256, m.IconBytes)
}

// MayWriteEntryMeta reports whether a caller may write an entry's metadata (the checks
// SetEntryMeta makes first): the API asks before it reads the body.
func (s *Service) MayWriteEntryMeta(ctx context.Context, a authz.Actor, tenant, packageType, source, name string) error {
	_, err := s.metaTarget(ctx, a, tenant, packageType, source, name, true)
	return err
}

// checkWrite checks a write against the entry's current metadata: a signed request older than the
// entry's mark, or of its second with another body (ErrStale), and an icon kept by a hash that is no
// longer the stored one (ErrIconChanged).
func checkWrite(cur store.EntryMeta, had bool, w MetaWrite, keep string) error {
	at := w.SignedAt.UTC().Truncate(time.Microsecond)
	if had && !w.SignedAt.IsZero() && (at.Before(cur.SignedAt) || at.Equal(cur.SignedAt) && w.BodySHA256 != cur.SignedBody) {
		return ErrStale
	}
	if keep != "" && cur.IconSHA256 != keep {
		return ErrIconChanged
	}
	return nil
}

// SetEntryMeta writes an entry's metadata, replacing what it had (spec 0036). A new icon is checked
// (internal/icon). Before anything is uploaded the write is checked against the entry's current
// metadata (stale, a kept icon changed), a publisher and its key against the store, and a new icon
// against the source's byte cap (catalogue.max_bytes, its growth only; a tenant's or a server
// administrator's write is not bounded); an icon the entry has already is not uploaded again. A new
// one is claimed (waiting at most IconClaimWait on the collector) and uploaded to the tenant's
// storage domain, and committed with the row. The row's transaction takes the tenant's auth lock and
// checks it all again. Writing the same metadata again changes nothing and makes no event; a signed
// write always advances the entry's mark, a token's that changes something moves it to the later of
// the server's clock and the mark, and a token's no-op writes nothing.
func (s *Service) SetEntryMeta(ctx context.Context, a authz.Actor, tenant, packageType, source, name string, w MetaWrite) (EntryMetaView, error) {
	x, err := s.metaTarget(ctx, a, tenant, packageType, source, name, true)
	if err != nil {
		return EntryMetaView{}, err
	}
	summary := ""
	if w.Summary != nil {
		if summary, err = CheckSummary(*w.Summary); err != nil {
			return EntryMetaView{}, err
		}
	}
	var data []byte
	var info icon.Info
	keep := ""
	switch {
	case w.Icon == nil:
	case w.Icon.Keep != "" || w.Icon.Data == nil && w.Icon.ContentType == "":
		if w.Icon.Data != nil || w.Icon.ContentType != "" {
			return EntryMetaView{}, fmt.Errorf("%w: an icon is new (content_type and data) or kept (sha256), not both", store.ErrInvalid)
		}
		if !sha256Hex.MatchString(w.Icon.Keep) {
			return EntryMetaView{}, fmt.Errorf("%w: a kept icon's sha256 is the stored icon's hash (64 lowercase hex digits)", store.ErrInvalid)
		}
		keep = w.Icon.Keep
	default:
		if w.Icon.ContentType != icon.ContentType {
			return EntryMetaView{}, fmt.Errorf("%w: the icon's content_type is %s", store.ErrInvalid, icon.ContentType)
		}
		if info, err = icon.Check(ctx, w.Icon.Data); errors.Is(err, icon.ErrInvalid) {
			return EntryMetaView{}, fmt.Errorf("%w: %s", store.ErrInvalid, strings.TrimPrefix(err.Error(), icon.ErrInvalid.Error()+": "))
		} else if err != nil {
			return EntryMetaView{}, err
		}
		data = w.Icon.Data
	}
	// the checks the row's transaction makes again, made before anything is uploaded
	cur, had, err := s.Store.EntryMeta(ctx, x.t.ID, x.typ, x.src, name)
	if err != nil {
		return EntryMetaView{}, err
	}
	if data != nil && blob.ContentHash(data) == cur.IconSHA256 {
		keep, data = cur.IconSHA256, nil // the icon the entry has: nothing to upload
	}
	if err := checkWrite(cur, had, w, keep); err != nil {
		return EntryMetaView{}, err
	}
	if a.Kind == authz.ActorPublisher {
		if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error { return tx.PublicationLive(ctx, x.src, w.KeyID) }); err != nil {
			return EntryMetaView{}, err
		}
	}
	if data != nil && (a.Kind == authz.ActorPublisher || a.Kind == authz.ActorDistributionAdmin) && s.MaxBytes > 0 {
		// read outside any lock, as room's: writes at once may pass it together
		_, used, err := s.Store.SourceUsage(ctx, x.t.ID, x.src)
		if err != nil {
			return EntryMetaView{}, err
		}
		if growth := int64(len(data)) - cur.IconBytes; growth > 0 && used+growth > s.MaxBytes {
			return EntryMetaView{}, fmt.Errorf("%w (catalogue.max_bytes): the source's builds placed nowhere and its entries' icons "+
				"hold as many bytes as it may", store.ErrCatalogueFull)
		}
	}
	if data == nil {
		return s.commitMeta(ctx, a, x, w, summary, keep, "", "", info, 0)
	}
	for attempt := 0; ; attempt++ {
		sha, claim, err := s.Blob.PutIcon(ctx, x.t.StorageDomain, data)
		if err != nil {
			return EntryMetaView{}, err
		}
		v, err := s.commitMeta(ctx, a, x, w, summary, "", sha, claim, info, int64(len(data)))
		if errors.Is(err, store.ErrClaimLost) && attempt < 2 {
			continue // claim again, upload again
		}
		if err != nil {
			_ = s.Blob.ReleaseIcon(context.WithoutCancel(ctx), x.t.StorageDomain, sha, claim)
			err = &UploadedError{Err: err}
		}
		return v, err
	}
}

// UploadedError is a metadata write that failed after its icon was uploaded (the object is left
// for the collector): the API records it.
type UploadedError struct{ Err error }

func (e *UploadedError) Error() string { return e.Err.Error() }
func (e *UploadedError) Unwrap() error { return e.Err }

// commitMeta writes the row (step 4 of an icon's write when sha is a new icon's), under the
// tenant's auth lock, checking again what the write was checked against before.
func (s *Service) commitMeta(ctx context.Context, a authz.Actor, x metaTarget, w MetaWrite, summary, keep, sha, claim string,
	info icon.Info, size int64) (EntryMetaView, error) {
	if s.beforeMetaCommit != nil {
		s.beforeMetaCommit()
	}
	locks := []string{store.PublisherAuthLock(x.t.ID)}
	if sha != "" {
		locks = append(locks, store.IconLock(x.t.StorageDomain, sha))
	}
	var out EntryMetaView
	err := s.Store.InTenantTx(ctx, x.t.ID, locks, func(tx *store.Tx) error {
		if !serverActor(a) {
			if active, err := tx.TenantActive(ctx, x.t.ID); err != nil {
				return err
			} else if !active {
				return authz.ErrDenied // suspended meanwhile
			}
		}
		switch a.Kind {
		case authz.ActorPublisher:
			if err := tx.PublicationLive(ctx, x.src, w.KeyID); err != nil {
				return err // the publisher or its key removed meanwhile: nothing is written
			}
			if x.dt != "" {
				if err := distributorMayNow(ctx, tx, x.t.ID, x.typ, x.name, x.dt); err != nil {
					return err // the name reserved meanwhile for another holder
				}
			}
		case authz.ActorPrincipal:
			if dt, err := tx.SourceDistributor(ctx, x.t.ID, x.src); err != nil {
				return err
			} else if dt != "" {
				return authz.ErrDenied // a tenant's administrators never write a distributor's entry
			}
		}
		cur, had, err := tx.EntryMeta(ctx, x.t.ID, x.typ, x.src, x.name)
		if err != nil {
			return err
		}
		if err := checkWrite(cur, had, w, keep); err != nil {
			return err
		}
		next := store.EntryMeta{TenantID: x.t.ID, PackageType: x.typ, Source: x.src, Name: x.name, Summary: summary}
		switch {
		case keep != "":
			next.IconSHA256, next.IconWidth, next.IconHeight, next.IconBytes = cur.IconSHA256, cur.IconWidth, cur.IconHeight, cur.IconBytes
		case sha != "":
			if err := tx.CommitIcon(ctx, x.t.StorageDomain, sha, claim); err != nil {
				return err
			}
			next.IconSHA256, next.IconWidth, next.IconHeight, next.IconBytes = sha, info.Width, info.Height, size
		}
		changed := next.Summary != cur.Summary || next.IconSHA256 != cur.IconSHA256
		signed := !w.SignedAt.IsZero()
		if !changed && !signed {
			out = x.view(cur, had) // a token's no-op writes nothing, its mark included
			return nil
		}
		next.SetBy, next.SetAt = cur.SetBy, cur.SetAt
		if changed || !had {
			next.SetBy, next.SetAt = a.String(), tx.Now().UTC().Truncate(time.Microsecond)
		}
		if signed {
			next.SignedAt, next.SignedBody = w.SignedAt.UTC().Truncate(time.Microsecond), w.BodySHA256
		} else {
			// a token's write: no signed request made before it undoes it, and the mark never goes
			// back (a signed write may have been dated ahead of the server's clock)
			next.SignedAt = tx.Now().UTC().Truncate(time.Microsecond)
			if cur.SignedAt.After(next.SignedAt) {
				next.SignedAt = cur.SignedAt
			}
		}
		if err := tx.PutEntryMeta(ctx, next); err != nil {
			return err
		}
		out = x.view(next, true)
		if !changed {
			return nil
		}
		f := map[string]any{"source": x.label, "name": x.name, "summary": next.Summary, "icon": next.IconSHA256, "icon_before": cur.IconSHA256}
		if w.KeyFingerprint != "" {
			f["key"] = w.KeyFingerprint
		}
		return tx.Event(ctx, x.t.ID, a.String(), "catalogue.meta", "ext:"+x.name, typedEvent(f, x.typ))
	})
	return out, err
}
