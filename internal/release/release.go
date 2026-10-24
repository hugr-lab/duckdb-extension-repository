package release

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/audit"
	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/blob"
	"github.com/hugr-lab/duckdb-extension-repository/internal/extfile"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/reserved"
	"github.com/hugr-lab/duckdb-extension-repository/internal/signer"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
	"github.com/hugr-lab/duckdb-extension-repository/internal/upstream/duckdbkeys"
)

// Errors.
var (
	ErrState = errors.New("release: not allowed in this state")
	ErrSlot  = errors.New("release: the slot holds another body; a fix is a new version")
	// ErrBlocked is a body banned in the tenant (spec 0008).
	ErrBlocked = errors.New("release: the body is blocked in this tenant")
	// ErrShadowed is an upstream build of a name a replacement holds in the channel (spec 0009).
	ErrShadowed = errors.New("release: a replacement holds the name in this channel")
	// ErrConflict is an upstream build for a slot that holds another body (spec 0009).
	ErrConflict = errors.New("release: the slot holds another body")
	// ErrOtherSource is a release of a name another source holds in the channel (spec 0031); the
	// error is a *SourceError, which names the source.
	ErrOtherSource = errors.New("release: another source holds the name in this channel")
	// ErrPackageType is a build released, placed, promoted or followed into a channel of another
	// package type (spec 0034), or an intake of a type this server has none for.
	ErrPackageType = errors.New("release: the channel serves another package type")
	// ErrHeld is a held entry placed, promoted or published into a channel (spec 0033 phase A2): it
	// reaches a channel once its hold has passed, or placed early by a channel administrator; and a
	// publisher with a hold publishing on a channel's route, which publishes into the catalogue.
	ErrHeld = errors.New("release: held")
)

// channelType is a channel's package type for the lookups that come before authorization (spec
// 0034): the default for a channel that is not there, which the authorization or the read after it
// refuses.
func (s *Service) channelType(ctx context.Context, tenant, channel string) (string, error) {
	c, err := s.Store.GetChannel(ctx, tenant, channel)
	if errors.Is(err, store.ErrNotFound) {
		return pkgtype.Default, nil
	}
	return c.PackageType, err
}

// sameType refuses a build of one type into a channel of another (spec 0034: 409).
func sameType(c store.Channel, b store.Build) error {
	if b.PackageType != c.PackageType || !pkgtype.Known(c.PackageType) {
		return fmt.Errorf("%w: the channel %s serves %s, the build is %s", ErrPackageType, c.Name, c.PackageType, b.PackageType)
	}
	return nil
}

// SourceError is a release refused because another source holds its name in the channel (spec
// 0031): the channel's administrator switches the source to let it in. Source is as answers show it
// (upstream:<name>, publisher:<name>, tenant).
type SourceError struct{ Name, Source string }

func (e *SourceError) Error() string {
	return fmt.Sprintf("release: the channel serves %s from %s; its administrator switches the source to let this one in", e.Name, e.Source)
}

// Is makes a SourceError an ErrOtherSource.
func (e *SourceError) Is(target error) bool { return target == ErrOtherSource }

// sourceOf is a release's source by who brings it (spec 0031): a publisher's key, or the tenant.
func sourceOf(a authz.Actor) string {
	if a.Kind == authz.ActorPublisher {
		var ids []string
		for k := range a.Principals {
			if k.Kind == store.PrincipalPublisher {
				ids = append(ids, k.Value)
			}
		}
		if len(ids) > 0 {
			slices.Sort(ids)
			return store.PublisherSource(ids[0])
		}
	}
	return store.SourceTenant
}

// checkSource refuses a release of a name another source holds in the channel (spec 0031). A
// mirrored release (mirror: an upstream's intake, not a promotion) succeeds a removed upstream: it
// returns the removed one's source, which the caller moves to the upstream. selected says the name
// has a source the release keeps.
func checkSource(ctx context.Context, tx *store.Tx, channelID, name, source string, mirror bool) (selected bool, succeeds string, err error) {
	sel, had, err := tx.ChannelSourceOf(ctx, channelID, name)
	if err != nil || !had || sel.Source == source {
		return had, "", err
	}
	if mirror && strings.HasPrefix(source, store.SourceUpstreamPrefix) {
		removed, err := tx.UpstreamRemoved(ctx, sel.Source)
		if err != nil {
			return false, "", err
		}
		if removed {
			return false, sel.Source, nil
		}
	}
	label, err := tx.SourceLabel(ctx, sel.Source)
	if err != nil {
		return false, "", err
	}
	return true, "", &SourceError{Name: name, Source: label}
}

// SignerOpener opens a key's signer, checked against the stored key (keys.Service.OpenSigner).
type SignerOpener interface {
	OpenSigner(ctx context.Context, k store.Key) (signer.Signer, error)
}

// Service manages builds and releases.
type Service struct {
	Store   *store.Store
	Blob    *blob.Service
	Signers SignerOpener
	Authz   authz.Authorizer
	// CoreKeys replaces DuckDB's core keys (tests); nil is the pin's (spec 0009: shadows).
	CoreKeys []*rsa.PublicKey
	// MayPublish reports whether a private upstream's credential (spec 0009 phase 3), or a bundle
	// import's "bundle:<prefix>" pseudo-credential (spec 0014), allows the releases it brought into
	// the tenant to be public; nil allows none.
	MayPublish func(tenant, credential string) bool

	// MaxEntries and MaxBytes bound what one source holds in a tenant's catalogue (spec 0033:
	// catalogue.max_entries, catalogue.max_bytes); 0: no bound.
	MaxEntries int
	MaxBytes   int64

	// PurgeOff refuses purges (spec 0016 phase 2): replicas may still run a version that does not
	// know purged slots and would refill them. kista sets it while gc.interval is 0 (the rollout
	// not declared done).
	PurgeOff bool

	afterBuild   func(store.Build)     // tests: between finding the Build and inserting the release
	beforeResign func([]store.Release) // tests: between reading a re-sign batch and inserting it
}

// CredentialOf is the private upstream credential a release came through (its provenance's
// "credential", carried by promotion), "" for none.
func CredentialOf(r store.Release) string {
	if r.Origin != store.OriginUpstream && r.Origin != store.OriginPromotion && r.Origin != store.OriginPlacement || r.Provenance == "" {
		return ""
	}
	var p struct {
		Credential string `json:"credential"`
	}
	_ = json.Unmarshal([]byte(r.Provenance), &p)
	return p.Credential
}

// DuckDBCore reports whether a Build is DuckDB's own core build: its original signature verifies
// with DuckDB's core keys over its body (spec 0009). Anything else under a core name replaces it.
func (s *Service) DuckDBCore(b store.Build) bool {
	if len(b.OriginSignature) != extfile.SignatureSize {
		return false
	}
	h, err := parseHash(b.BodyHash)
	if err != nil {
		return false
	}
	keys := s.CoreKeys
	if keys == nil {
		keys = duckdbkeys.Core()
	}
	_, ok := extfile.Verify(h, b.OriginSignature, keys)
	return ok
}

func lockKey(channelID string) string { return "kista/channel/" + channelID }

// AddOptions are the choices of an add or a publication.
type AddOptions struct {
	Name string
	// PackageType is the type a publication into the catalogue names (spec 0034: ?type=, "": the
	// default); a channel's add takes the channel's.
	PackageType string
	Private     bool
	NotCurrent  bool
	// Unchecked skips spec 0008's file checks (prefix, format, entry point): the CLI's operator
	// vouches for the body. Never set over the API.
	Unchecked bool
	// Publish, when set, makes this a publication (spec 0008): the caller needs publish on the name,
	// the declared version and platform must be the footer's, and the checks always run.
	Publish *Publication
	// NoWait fails with blob.ErrBusy instead of waiting for an ingest slot.
	NoWait bool
}

// Publication is what a publisher declares about an upload.
type Publication struct {
	Version, Platform string
	Provenance        string // JSON, recorded on the release
	// Statement and Signature are the publisher's signed request (spec 0023), recorded on the release.
	Statement, Signature string
	// KeySources are the ids of the matched publishers marked as sources of licence keys (spec
	// 0021). Those that hold publish on the name themselves record the build's keys.
	KeySources []string
	// KeyID is the publisher's key that signed the request (spec 0023), recorded on a new build: revoking
	// it purges the entries it published that are still held (spec 0033 phase A2). "": none.
	KeyID string
}

func (o AddOptions) visibility() string {
	if o.Private {
		return store.Private
	}
	return store.Public
}

// Add puts a built extension into a signed channel: spool, check, commit, sign, release. It returns
// the release and whether it already existed (the same body in the same slot with the same choices).
func (s *Service) Add(ctx context.Context, a authz.Actor, tenant, channel string, r io.Reader, o AddOptions) (store.Release, bool, error) {
	res := authz.Resource{Tenant: tenant, Channel: channel}
	verb := authz.VerbAdmin
	if o.Publish != nil {
		typ, err := s.channelType(ctx, tenant, channel)
		if err != nil {
			return store.Release{}, false, err
		}
		kind, err := s.Store.ReservedFor(ctx, tenant, typ, o.Name) // spec 0030: DuckDB's, the tenant's, its upstreams'
		if err != nil {
			return store.Release{}, false, err
		}
		o.Unchecked = false
		verb = authz.VerbPublish
		res = authz.Resource{Tenant: tenant, Channel: channel, Package: o.Name, Reserved: kind != ""}
	}
	if err := s.Authz.Allow(ctx, a, verb, res); err != nil {
		return store.Release{}, false, err
	}
	if o.Publish != nil { // spec 0033 phase A2: a publisher with a hold publishes into the catalogue
		if hold, err := s.Store.SourceHold(ctx, sourceOf(a)); err != nil {
			return store.Release{}, false, err
		} else if hold > 0 {
			return store.Release{}, false, fmt.Errorf("%w: the publisher has a hold (%s): it publishes into the catalogue "+
				"(POST /api/v1/tenants/%s/catalogue/extensions/%s/builds), and channels take its builds by their rules once the hold has passed",
				ErrHeld, hold, tenant, o.Name)
		}
	}
	sc, err := s.Store.GetServeChannel(ctx, tenant, channel)
	if err != nil {
		return store.Release{}, false, err
	}
	ch := sc.Channel
	if ch.Kind != store.ChannelSigned {
		return store.Release{}, false, fmt.Errorf("%w: a %s channel is fed by upstreams (spec 0009)", ErrState, ch.Kind)
	}
	if ch.PackageType != pkgtype.DuckDBExtension { // the intake reads DuckDB extensions (spec 0008)
		return store.Release{}, false, fmt.Errorf("%w: the channel serves %s; this server takes DuckDB extensions only", ErrPackageType, ch.PackageType)
	}
	if _, err := activeKey(ctx, s.Store, ch.ID); err != nil {
		return store.Release{}, false, err
	}
	spool := s.Blob.Spool
	if o.NoWait {
		spool = s.Blob.SpoolNoWait
	}
	sp, err := spool(ctx, sc.Tenant.StorageDomain, r)
	if err != nil {
		return store.Release{}, false, err
	}
	defer sp.Close()
	b, err := BuildFrom(sp.File(), sc.Tenant.ID, o.Name, a.String())
	if err != nil {
		return store.Release{}, false, err
	}
	it := item{b: b, visibility: o.visibility(), notCurrent: o.NotCurrent, origin: store.OriginAdmin, source: sourceOf(a)}
	it.b.Source = it.source
	if p := o.Publish; p != nil {
		if p.Version != b.PackageVersion || p.Platform != b.Platform {
			return store.Release{}, false, fmt.Errorf("%w: the file's footer says version %q, platform %q; the request says %q, %q",
				store.ErrInvalid, b.PackageVersion, b.Platform, p.Version, p.Platform)
		}
		it.origin, it.provenance, it.statement, it.signature = store.OriginPublication, p.Provenance, p.Statement, p.Signature
		it.b.Origin, it.b.PublisherKeyID = store.OriginPublication, p.KeyID
		it.publication, it.keyID = true, p.KeyID
		if !res.Reserved {
			it.recheck = s.reservedCheck(a, authz.VerbPublish, res)
		}
		if err := s.publisherKeys(ctx, a, res, p.KeySources, sp.File(), &it); err != nil {
			return store.Release{}, false, err
		}
	}
	if !o.Unchecked {
		if err := CheckFile(sp.File(), o.Name); err != nil {
			return store.Release{}, false, err
		}
		it.b.Checked = true
	}
	if blocked, err := s.Store.Blocked(ctx, sc.Tenant.ID, b.BodyHash); err != nil || blocked {
		if err == nil {
			err = ErrBlocked
		}
		return store.Release{}, false, err
	}
	// check the slot before storing a body that would be refused
	var existing *store.Release
	selected := false // the name has a source the release keeps (spec 0031)
	if err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		if selected, _, err = checkSource(ctx, tx, ch.ID, b.Name, it.source, false); err != nil {
			return err
		}
		cands, err := tx.SlotCandidates(ctx, ch.ID, it.source, b.Name, b.PackageVersion, b.Platform)
		if err != nil {
			return err
		}
		existing, err = checkSlot(cands, it)
		return err
	}); err != nil {
		return store.Release{}, false, err
	}
	// a name without a source goes through the insert, which sets it (spec 0031)
	if existing != nil && selected {
		// a body marked corrupt (spec 0005, 0018) is uploaded again: adding the same file repairs it
		if rec, err := s.Store.GetBlob(ctx, sc.Tenant.StorageDomain, b.BodyHash); err == nil && !rec.CorruptAt.IsZero() {
			cctx, cancel := context.WithTimeout(ctx, blob.IntakeDeadline)
			_, err := sp.Commit(cctx)
			cancel()
			if err != nil {
				return store.Release{}, false, err
			}
		}
		// publishing the same body with the checks marks its Build checked, and a marked publisher's
		// publication records its keys (spec 0021)
		// a publication finding its release still needs its publisher and key (spec 0033 phase A2; a
		// publication is always checked)
		if it.b.Checked || len(it.keySources) > 0 {
			bb := it.b
			if err := s.Store.InTx(ctx, buildLock(bb), func(tx *store.Tx) error {
				if it.publication {
					if err := tx.PublicationLive(ctx, it.source, it.keyID); err != nil {
						return err
					}
				}
				if err := tx.FindOrInsertBuild(ctx, &bb); err != nil {
					return err
				}
				return recordKeys(ctx, tx, bb, it)
			}); err != nil {
				return store.Release{}, false, err
			}
		}
		return *existing, true, nil
	}
	// the channel must serve the build before its body is stored (re-checked under the lock)
	capis, err := s.Store.ChannelCAPIs(ctx, ch.ID)
	if err != nil {
		return store.Release{}, false, err
	}
	if err := servable(capis, b); err != nil {
		return store.Release{}, false, err
	}
	if err := s.Store.SlotTaken(ctx, it.b); err != nil { // spec 0033: before the body is stored
		return store.Release{}, false, slotErr(it.origin, err)
	}
	if err := s.room(ctx, it.b, sp.File().BodySize, o.Publish == nil); err != nil { // spec 0033: the source's catalogue
		return store.Release{}, false, err
	}
	kind := audit.Kind("release.add")
	if it.origin == store.OriginPublication {
		kind = "release.publish"
	}
	rs, existed, err := s.commitAndRelease(ctx, sp, &it, func(ctx context.Context, it item) ([]store.Release, bool, error) {
		return s.insert(ctx, a, ch.ID, []item{it}, func(tx *store.Tx, c store.Channel, made []store.Release) error {
			r := made[0]
			f := releaseFields(r, it.b, kind == "release.publish")
			f["shadows"] = Shadows(c.PackageType, store.Candidate{Release: r, BuildOrigin: it.b.Origin}, nil)
			return tx.Event(ctx, c.TenantID, a.String(), kind, releaseSubject(c, r), f)
		})
	})
	if err != nil {
		return store.Release{}, false, err
	}
	return rs[0], existed, nil
}

// item is one release to insert: a Build and the release's choices.
type item struct {
	b                  store.Build
	visibility         string
	notCurrent         bool
	origin, provenance string
	// source is whose release it is (spec 0031): the channel takes it only from the name's source
	source string
	// statement and signature are a publisher's signed request (spec 0023)
	statement, signature string
	originSignature      []byte // an upstream release's (spec 0009)
	// keys are the build's licence keys, recorded for each of keySources, of kind keyKind (spec 0021)
	keys       []store.LicenseKey
	keyKind    string
	keySources []string
	// recheck authorizes again as for a reserved name; insert calls it under the locks when an
	// upstream of the tenant provides the name (spec 0009), or the server reserved it since (spec
	// 0030). Nil: no check.
	recheck func(context.Context) error
	// reserveKind reserves the name with reserveSource when the release is inserted (spec 0030: a
	// build mirrored from DuckDB's repositories). "": none.
	reserveKind, reserveSource string
	// anyKind is the upstream's kind when its "*" took the name: insert refuses the name under the
	// locks if it is reserved since (as AnyTakes, and the tenant's names and upstreams). "": none.
	anyKind string
	// publication is a publisher's publication on a channel's route, signed with keyID ("": none):
	// insert checks under its locks that the publisher and the key still exist (spec 0033 phase A2:
	// a key revoked, or a publisher removed, while the request was in flight releases nothing).
	publication bool
	keyID       string
	// auto is an automatic placement (spec 0033: a rule's, a follow's, a mirror's run): it waits for
	// the channel's hold too. early places a held entry (a channel administrator's, audited); insert
	// refuses a held entry under the channel lock otherwise.
	auto, early bool
}

// reservedCheck is an item's recheck: the verb on the resource as a reserved name.
func (s *Service) reservedCheck(a authz.Actor, verb authz.Verb, res authz.Resource) func(context.Context) error {
	res.Reserved = true
	return func(ctx context.Context) error { return s.Authz.Allow(ctx, a, verb, res) }
}

// checkUpstreamSlot finds an upstream item's slot (spec 0009): the release of the same body in any
// state with any choices is the existing one, so an administrator's changes survive every run;
// another body, a purged slot (spec 0016), or an ABI the slot's releases do not mix with, is a
// conflict.
func checkUpstreamSlot(cands []store.Candidate, b store.Build) (*store.Release, error) {
	for _, c := range cands {
		if (c.ABI == store.ABICStruct) != (b.ABI == store.ABICStruct) {
			return nil, fmt.Errorf("%w: %s %s on %s has %s releases%s", ErrConflict, b.Name, b.PackageVersion, b.Platform, c.ABI, purgedNote(c))
		}
		if c.Slot != b.Slot() {
			continue
		}
		if c.BodyHash != b.BodyHash || c.State == store.ReleasePurged {
			return nil, fmt.Errorf("%w (%s %s %s %s%s)", ErrConflict, b.Name, b.PackageVersion, b.Platform, c.Slot, purgedNote(c))
		}
		r := c.Release
		return &r, nil
	}
	return nil, nil
}

func purgedNote(c store.Candidate) string {
	if c.State == store.ReleasePurged {
		return ", purged"
	}
	return ""
}

// checkSlot finds the release in its slot. The same body with the same choices is the existing
// release; another body, other choices, or mixing c_struct with cpp builds are refused.
func checkSlot(cands []store.Candidate, it item) (*store.Release, error) {
	b := it.b
	for _, c := range cands {
		if (c.ABI == store.ABICStruct) != (b.ABI == store.ABICStruct) {
			return nil, fmt.Errorf("%w: %s %s on %s already has %s releases%s; c_struct and exact-version builds do not mix",
				ErrSlot, b.Name, b.PackageVersion, b.Platform, c.ABI, purgedNote(c))
		}
		if c.Slot != b.Slot() {
			continue
		}
		if c.State == store.ReleasePurged {
			return nil, fmt.Errorf("%w: %s %s %s %s was purged; a fix is a new version", ErrSlot, b.Name, b.PackageVersion, b.Platform, c.Slot)
		}
		if c.BodyHash != b.BodyHash {
			return nil, fmt.Errorf("%w (%s %s %s %s)", ErrSlot, b.Name, b.PackageVersion, b.Platform, c.Slot)
		}
		if c.State == store.ReleaseYanked {
			return nil, fmt.Errorf("%w: %s %s %s %s holds this build and is yanked; a fix is a new version",
				ErrSlot, b.Name, b.PackageVersion, b.Platform, c.Slot)
		}
		if c.Visibility != it.visibility || (c.Seq == 0) != it.notCurrent {
			return nil, fmt.Errorf("%w: %s %s %s %s holds this build as %s/%s; change it with release public|private or release current",
				ErrState, b.Name, b.PackageVersion, b.Platform, c.Slot, c.State, c.Visibility)
		}
		r := c.Release
		return &r, nil
	}
	return nil, nil
}

func activeKey(ctx context.Context, st *store.Store, channelID string) (store.Key, error) {
	keys, err := st.ListKeys(ctx, channelID)
	if err != nil {
		return store.Key{}, err
	}
	for _, k := range keys {
		if k.State == store.KeyActive {
			return k, nil
		}
	}
	return store.Key{}, fmt.Errorf("%w: the channel has no active key", ErrState)
}

// sign signs a body hash with a key and verifies the result against the key's stored public key.
func (s *Service) sign(ctx context.Context, k store.Key, h extfile.BodyHash) ([]byte, error) {
	sg, err := s.Signers.OpenSigner(ctx, k)
	if err != nil {
		return nil, err
	}
	sig, err := sg.Sign(ctx, h)
	if err != nil {
		return nil, err
	}
	pub, err := x509.ParsePKIXPublicKey(k.PublicKey)
	rsaPub, ok := pub.(*rsa.PublicKey)
	if err != nil || !ok {
		return nil, errors.New("release: the stored key is not an RSA key")
	}
	if _, ok := extfile.Verify(h, sig, []*rsa.PublicKey{rsaPub}); !ok {
		return nil, errors.New("release: the signer's signature does not verify against the stored key")
	}
	return sig, nil
}

func parseHash(hexHash string) (extfile.BodyHash, error) {
	var h extfile.BodyHash
	if len(hexHash) != 64 {
		return h, errors.New("release: bad body hash")
	}
	for i := range h {
		var v byte
		for _, c := range hexHash[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				v |= byte(c-'a') + 10
			default:
				return h, errors.New("release: bad body hash")
			}
		}
		h[i] = v
	}
	return h, nil
}

// required are the keys a new release must be signed by: the active key and the serving key (the
// active key when the channel has none yet).
func required(keys []store.Key, serving string) ([]store.Key, error) {
	var out []store.Key
	for _, k := range keys {
		if k.State == store.KeyActive || k.ID == serving {
			out = append(out, k)
		}
	}
	if !slices.ContainsFunc(out, func(k store.Key) bool { return k.State == store.KeyActive }) {
		return nil, fmt.Errorf("%w: the channel has no active key", ErrState)
	}
	return out, nil
}

// insert signs outside any transaction and inserts the releases under the channel lock, all or
// none, retrying while the keys it needs change underneath. It returns the releases (existing ones
// for items whose slot already holds them with the same choices) and whether all of them existed.
// eventFn writes the event of the releases an insert made (spec 0010), in its transaction.
type eventFn func(tx *store.Tx, c store.Channel, made []store.Release) error

func (s *Service) insert(ctx context.Context, a authz.Actor, channelID string, items []item, event eventFn) ([]store.Release, bool, error) {
	hashes := make([]extfile.BodyHash, len(items))
	for i, it := range items {
		h, err := parseHash(it.b.BodyHash)
		if err != nil {
			return nil, false, err
		}
		hashes[i] = h
	}
	sigs := make([]map[string][]byte, len(items))
	for i := range sigs {
		sigs[i] = map[string][]byte{}
	}
	keys, err := s.Store.ListKeys(ctx, channelID)
	if err != nil {
		return nil, false, err
	}
	ch, err := s.channelByID(ctx, channelID)
	if err != nil {
		return nil, false, err
	}
	// a passthrough channel's releases carry DuckDB's signature only (spec 0009): no keys
	passthrough := ch.Kind == store.ChannelPassthrough
	var need []store.Key
	if !passthrough {
		if need, err = required(keys, ch.ServingKeyID); err != nil {
			return nil, false, err
		}
	}
	for attempt := 0; attempt < 4; attempt++ {
		for i := range items {
			for _, k := range need {
				if sigs[i][k.ID] != nil {
					continue
				}
				sig, err := s.sign(ctx, k, hashes[i])
				if err != nil {
					if k.State != store.KeyActive {
						return nil, false, fmt.Errorf("signing with the serving key %s (the old one until a re-sign moves it: kista admin key resign): %w", k.Fingerprint, err)
					}
					return nil, false, err
				}
				sigs[i][k.ID] = sig
			}
		}
		var out, made []store.Release
		existing := 0
		need = nil
		// a signed channel's inserts take the tenant's upstream lock first (spec 0009): the reservation
		// is read again, and a tenant shadow recorded, under it
		locks := []string{lockKey(channelID)}
		if !passthrough {
			locks = []string{store.TenantUpstreamLock(ch.TenantID), lockKey(channelID)}
		}
		err := s.Store.InTxLocks(ctx, locks, func(tx *store.Tx) error {
			out, made, existing, need = nil, nil, 0, nil
			bump := false    // a cleared source set again by its release (spec 0031)
			learned := false // a name reserved by this intake (spec 0030)
			c, err := tx.ChannelByID(ctx, channelID)
			if err != nil {
				return err
			}
			for _, it := range items { // spec 0034: a channel takes builds of its type only
				if err := sameType(c, it.b); err != nil {
					return err
				}
			}
			keys, err := tx.ChannelKeys(ctx, channelID)
			if err != nil {
				return err
			}
			var req []store.Key
			if c.Kind != store.ChannelPassthrough {
				if req, err = required(keys, c.ServingKeyID); err != nil {
					return err
				}
			}
			for _, k := range req {
				for i := range items {
					if sigs[i][k.ID] == nil {
						need = append(need, k)
						break
					}
				}
			}
			if len(need) > 0 {
				return errResign
			}
			live := map[string]bool{}
			for _, k := range keys {
				live[k.ID] = k.State != store.KeyRetired
			}
			capis, err := tx.ChannelCAPIs(ctx, channelID)
			if err != nil {
				return err
			}
			for i, it := range items {
				b := it.b
				// spec 0008: a body blocked after the caller's first check is never released
				if blocked, err := tx.Blocked(ctx, b.TenantID, b.BodyHash); err != nil || blocked {
					if err == nil {
						err = ErrBlocked
					}
					return err
				}
				if it.reserveKind != "" {
					changed, err := tx.ReserveName(ctx, b.PackageType, b.Name, it.reserveKind, it.reserveSource)
					if err != nil {
						return err
					}
					learned = learned || changed
				}
				if it.anyKind != "" {
					taken, err := anyTakesNow(ctx, tx, b.TenantID, b.PackageType, b.Name, it.anyKind)
					if err != nil {
						return err
					}
					if !taken {
						return ErrShadowed
					}
				}
				upstream := it.origin == store.OriginUpstream
				if it.recheck != nil {
					// a name an upstream of the tenant provides, or one the server or the tenant reserved
					// since the caller was authorized (spec 0030), takes a grant that names it
					provided, err := tx.UpstreamProvides(ctx, b.TenantID, b.PackageType, b.Name)
					if err != nil {
						return err
					}
					if !provided {
						k, err := tx.ReservedKind(ctx, b.PackageType, b.Name)
						if err != nil {
							return err
						}
						provided = k != ""
					}
					if !provided {
						if provided, err = tx.TenantReserves(ctx, b.TenantID, b.PackageType, b.Name); err != nil {
							return err
						}
					}
					if provided {
						if err := it.recheck(ctx); err != nil {
							return err
						}
					}
				}
				// spec 0033 phase A2: a publication's publisher and key, read with shared row locks a removal
				// waits for; after the recheck, which may read the tenant's auth on another connection
				// (GetTenantAuth's locked read), so no removal waits on a row this transaction holds then
				if it.publication {
					if err := tx.PublicationLive(ctx, it.source, it.keyID); err != nil {
						return err
					}
				}
				// spec 0031: only the name's source in the channel lands; the first sets it, and an
				// upstream succeeds a removed one
				selected, succeeds, err := checkSource(ctx, tx, channelID, b.Name, it.source, upstream)
				if err != nil {
					return err
				}
				if succeeds != "" {
					if err := setSource(ctx, tx, a, c, b.Name, it.source, succeeds); err != nil {
						return err
					}
					// the removed upstream's releases, yanks and purges are the successor's (spec 0033)
					if _, err := tx.TakeReleases(ctx, channelID, b.Name, succeeds, it.source); err != nil {
						return err
					}
					selected = true
				}
				if it.b.Source != it.source {
					return fmt.Errorf("release: a build of %s released as %s's", it.b.Source, it.source)
				}
				cands, err := tx.SlotCandidates(ctx, channelID, it.source, b.Name, b.PackageVersion, b.Platform)
				if err != nil {
					return err
				}
				check := checkSlot
				if upstream {
					check = func(cands []store.Candidate, it item) (*store.Release, error) { return checkUpstreamSlot(cands, it.b) }
				}
				ex, err := check(cands, it)
				if err != nil {
					return err
				}
				if ex != nil {
					if !selected { // spec 0031: the same body arriving again sets a cleared source
						if err := setSource(ctx, tx, a, c, b.Name, it.source, ""); err != nil {
							return err
						}
						bump = true
					}
					out = append(out, *ex)
					existing++
					continue
				}
				if err := servable(capis, b); err != nil {
					return err
				}
				if !it.early { // spec 0033 phase A2: a held entry reaches no channel
					if err := heldNow(ctx, tx, c, it); err != nil {
						return err
					}
				}
				r := store.Release{TenantID: b.TenantID, ChannelID: channelID, BuildID: b.ID, Name: b.Name, Source: it.source,
					PackageVersion: b.PackageVersion, Platform: b.Platform, Slot: b.Slot(), State: store.ReleaseActive,
					Visibility: it.visibility, Origin: it.origin, Provenance: it.provenance, OriginSignature: it.originSignature,
					PublisherStatement: it.statement, PublisherSignature: it.signature}
				if !it.notCurrent {
					if r.Seq, err = tx.NextSeq(ctx, channelID); err != nil {
						return err
					}
				}
				if err := tx.InsertRelease(ctx, &r, a.String()); err != nil {
					return err
				}
				for id, sig := range sigs[i] {
					if live[id] {
						if err := tx.InsertSignature(ctx, r.ID, channelID, id, sig); err != nil {
							return err
						}
					}
				}
				if !selected {
					if err := setSource(ctx, tx, a, c, b.Name, it.source, ""); err != nil {
						return err
					}
				}
				// any body other than DuckDB's own core build, under a core name or a name the tenant's
				// passthrough channels serve, released in a signed channel of DuckDB extensions, shadows
				// DuckDB's build in those channels until an administrator removes the shadow (spec 0009,
				// spec 0034)
				if c.Kind == store.ChannelSigned && c.PackageType == pkgtype.DuckDBExtension && !s.DuckDBCore(b) {
					k, err := tx.ReservedKind(ctx, b.PackageType, b.Name) // the server's list as it is now (spec 0030)
					if err != nil {
						return err
					}
					core := k == reserved.Core
					if !core {
						if core, err = tx.PassthroughProvides(ctx, b.TenantID, b.Name); err != nil {
							return err
						}
					}
					if core {
						added, err := tx.AddShadow(ctx, b.TenantID, b.Name, a.String())
						if err != nil {
							return err
						}
						if added {
							if err := tx.Event(ctx, b.TenantID, a.String(), "shadow.add", "shadow:"+b.Name, map[string]any{"name": b.Name}); err != nil {
								return err
							}
						}
					}
				}
				out = append(out, r)
				made = append(made, r)
			}
			// spec 0021: the keys a trusted intake carried, with its release, in one transaction
			for _, it := range items {
				if err := recordKeys(ctx, tx, it.b, it); err != nil {
					return err
				}
			}
			if learned { // spec 0030: after the names, so they are locked before the version
				if err := tx.BumpReservedVersion(ctx); err != nil {
					return err
				}
			}
			if existing == len(items) {
				if bump {
					return tx.BumpReleaseVersion(ctx, channelID)
				}
				return nil
			}
			if event != nil {
				if err := event(tx, c, made); err != nil {
					return err
				}
			}
			if c.ServingKeyID == "" {
				for _, k := range req {
					if k.State == store.KeyActive {
						if err := tx.SetServingKey(ctx, channelID, "", k.ID); err != nil {
							return err
						}
					}
				}
			}
			return tx.BumpReleaseVersion(ctx, channelID)
		})
		if errors.Is(err, errResign) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		return out, existing == len(items), nil
	}
	return nil, false, errors.New("release: the channel's keys kept changing; try again")
}

// heldNow refuses an item's entry when it is held now, or revoked (spec 0033 phase A2: its key was
// revoked with purge_held, or its publisher removed, while it was placed early), read under the
// channel lock (a hold raised meanwhile counts); an automatic placement waits for the channel's hold
// too.
func heldNow(ctx context.Context, tx *store.Tx, c store.Channel, it item) error {
	created, available, revoked, err := tx.BuildAvailability(ctx, it.b.ID)
	if err != nil {
		return err
	}
	if !revoked.IsZero() {
		return fmt.Errorf("%w: %s %s %s was revoked (its publisher key or publisher was removed while it was held); "+
			"only a channel administrator places it (early)", ErrHeld, it.b.Name, it.b.PackageVersion, it.b.Platform)
	}
	var hold time.Duration
	if it.auto {
		hold = c.Hold
	}
	if at := (store.Build{CreatedAt: created, AvailableAt: available}).AvailableIn(hold); at.After(tx.Now()) {
		return fmt.Errorf("%w: %s %s %s is held until %s", ErrHeld, it.b.Name, it.b.PackageVersion, it.b.Platform, at.UTC().Format(time.RFC3339))
	}
	return nil
}

// setSource sets a name's source in a channel on an arrival (spec 0031): the first release of the
// name (a rule without a mode, spec 0033: the channel serves what lands), or an upstream succeeding
// a removed one (before: its source; the rule's mode kept). The event names the sources as answers
// do.
func setSource(ctx context.Context, tx *store.Tx, a authz.Actor, c store.Channel, name, source, before string) error {
	if _, err := tx.SetChannelRule(ctx, c.ID, name, source, store.RuleNone, a.String()); err != nil {
		return err
	}
	label, err := tx.SourceLabel(ctx, source)
	if err != nil {
		return err
	}
	if before != "" {
		if before, err = tx.SourceLabel(ctx, before); err != nil {
			return err
		}
	}
	return tx.Event(ctx, c.TenantID, a.String(), "source.set", "channel:"+c.Name+"/ext:"+name,
		map[string]any{"name": name, "source": label, "before": before, "arrival": true})
}

// servable checks that a channel serves a build: its exact DuckDB version (cpp, c_struct_unstable),
// or a DuckDB version whose C API maximum accepts it (c_struct).
func servable(capis map[string][]store.CAPI, b store.Build) error {
	if b.ABI == store.ABICStruct {
		if b.CAPI == nil || !acceptsAny(capis, *b.CAPI) {
			return fmt.Errorf("%w: no DuckDB version the channel serves accepts C API %s (%s %s %s)",
				store.ErrInvalid, b.CAPI, b.Name, b.PackageVersion, b.Platform)
		}
		return nil
	}
	if _, ok := capis[b.DuckDBVersion]; !ok {
		return fmt.Errorf("%w: the channel does not serve DuckDB %s (%s %s %s); add the version to it first",
			store.ErrInvalid, b.DuckDBVersion, b.Name, b.PackageVersion, b.Platform)
	}
	return nil
}

// acceptsAny reports whether a DuckDB version of the channel accepts a c_struct build's C API.
func acceptsAny(capis map[string][]store.CAPI, c store.CAPI) bool {
	for _, maxima := range capis {
		for _, m := range maxima {
			if m.Accepts(c) {
				return true
			}
		}
	}
	return false
}

var errResign = errors.New("release: keys changed")

func (s *Service) channelByID(ctx context.Context, id string) (store.Channel, error) {
	var c store.Channel
	err := s.Store.InTx(ctx, "", func(tx *store.Tx) error {
		var err error
		c, err = tx.ChannelByID(ctx, id)
		return err
	})
	return c, err
}

// List lists a channel's releases (optionally of one name: an administrator of that extension may,
// and its publishers, spec 0008), newest first.
func (s *Service) List(ctx context.Context, a authz.Actor, tenant, channel, name string) ([]store.Candidate, error) {
	if err := s.mayRead(ctx, a, tenant, channel, name); err != nil {
		return nil, err
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return nil, err
	}
	return s.Store.ListReleases(ctx, ch.ID, name)
}

// mayRead allows an extension's administrators and, for one name, its publishers.
func (s *Service) mayRead(ctx context.Context, a authz.Actor, tenant, channel, name string) error {
	res := authz.Resource{Tenant: tenant, Channel: channel, Package: name}
	err := s.Authz.Allow(ctx, a, authz.VerbAdmin, res)
	if name == "" || !errors.Is(err, authz.ErrDenied) {
		return err
	}
	typ, err := s.channelType(ctx, tenant, channel)
	if err != nil {
		return err
	}
	kind, err := s.Store.ReservedFor(ctx, tenant, typ, name) // spec 0030
	if err != nil {
		return err
	}
	res.Reserved = kind != ""
	for _, v := range []authz.Verb{authz.VerbPublish, authz.VerbPromote} {
		if err = s.Authz.Allow(ctx, a, v, res); !errors.Is(err, authz.ErrDenied) {
			return err
		}
	}
	return err
}

// Change is a change to one release.
type Change string

// Changes.
const (
	Yank        Change = "yank"
	Deprecate   Change = "deprecate"
	Activate    Change = "activate"
	MakeCurrent Change = "current"
	SetPublic   Change = "public"
	SetPrivate  Change = "private"
)

// Apply changes one release of a channel. With a name, the release must be of that extension (an
// administrator of the extension may change it); a non-zero expected version must be the release's.
func (s *Service) Apply(ctx context.Context, a authz.Actor, tenant, channel, name, id string, c Change, expected int64) (store.Release, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel, Package: name}); err != nil {
		return store.Release{}, err
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return store.Release{}, err
	}
	var out store.Release
	err = s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		r, err := tx.GetRelease(ctx, ch.ID, id)
		if err != nil {
			return err
		}
		if name != "" && r.Name != name {
			return fmt.Errorf("%w: release %s", store.ErrNotFound, id) // another extension's id
		}
		if expected != 0 && r.Version != expected {
			return store.ErrConflict
		}
		refuse := func() error { return fmt.Errorf("%w: %q is not allowed on a %s release", ErrState, c, r.State) }
		actor := a.String()
		switch c {
		case Yank:
			if r.State == store.ReleaseYanked {
				return refuse()
			}
			err = tx.SetReleaseState(ctx, &r, store.ReleaseYanked, actor)
		case Deprecate:
			if r.State != store.ReleaseActive {
				return refuse()
			}
			err = tx.SetReleaseState(ctx, &r, store.ReleaseDeprecated, actor)
		case Activate:
			if r.State != store.ReleaseDeprecated {
				return refuse()
			}
			err = tx.SetReleaseState(ctx, &r, store.ReleaseActive, actor)
		case MakeCurrent:
			if r.State != store.ReleaseActive {
				return refuse()
			}
			var seq int64
			if seq, err = tx.NextSeq(ctx, ch.ID); err == nil {
				err = tx.SetReleaseSeq(ctx, &r, seq, actor)
			}
		case SetPublic, SetPrivate:
			if r.State == store.ReleaseYanked {
				return refuse()
			}
			// a subscription's extensions are not the tenant's to publish (spec 0009 phase 3)
			if cr := CredentialOf(r); c == SetPublic && cr != "" && (s.MayPublish == nil || !s.MayPublish(tenant, cr)) {
				return fmt.Errorf("%w: the release came through credential %s, which does not allow public releases", ErrState, cr)
			}
			if c == SetPrivate && ch.Kind == store.ChannelPassthrough {
				return fmt.Errorf("%w: a passthrough channel's releases are public", ErrState)
			}
			err = tx.SetReleaseVisibility(ctx, &r, string(c), actor)
		default:
			return fmt.Errorf("%w: unknown change %q", store.ErrInvalid, c)
		}
		if err != nil {
			return err
		}
		out = r
		kind := map[Change]audit.Kind{Yank: "release.yank", Deprecate: "release.deprecate", Activate: "release.activate",
			MakeCurrent: "release.current", SetPublic: "release.public", SetPrivate: "release.private"}[c]
		if err := tx.Event(ctx, ch.TenantID, a.String(), kind, releaseSubject(ch, r),
			map[string]any{"release": r.ID, "name": r.Name, "version": r.PackageVersion, "platform": r.Platform}); err != nil {
			return err
		}
		return tx.BumpReleaseVersion(ctx, ch.ID)
	})
	return out, err
}

func releaseSubject(c store.Channel, r store.Release) string {
	return "channel:" + c.Name + "/ext:" + r.Name + "/release:" + r.ID
}

// releaseFields are a release's facts in its events (spec 0010); provenance, which a caller
// controls, is one string.
func releaseFields(r store.Release, b store.Build, provenance bool) map[string]any {
	f := map[string]any{"release": r.ID, "name": r.Name, "version": r.PackageVersion, "platform": r.Platform, "slot": r.Slot,
		"body_hash": b.BodyHash, "visibility": r.Visibility, "current": r.Seq != 0}
	if provenance && r.Provenance != "" {
		f["provenance"] = r.Provenance
	}
	return f
}

// commitAndRelease commits a spooled body, finds or inserts its Build and inserts its release, all
// under the intake deadline (spec 0016: what the intake touched stays live that long); a Build the
// collector removed in between starts it again from the commit, once.
func (s *Service) commitAndRelease(ctx context.Context, sp *blob.Spool, it *item,
	ins func(ctx context.Context, it item) ([]store.Release, bool, error)) ([]store.Release, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, blob.IntakeDeadline)
	defer cancel()
	for attempt := 0; ; attempt++ {
		if _, err := sp.Commit(ctx); err != nil {
			return nil, false, err
		}
		// two intakes of one body race on the build's insert; the loser finds the winner's build
		for try := 0; ; try++ {
			bb := it.b
			bb.Visibility, bb.Provenance = it.visibility, it.provenance // spec 0033: as its arrival made its release
			err := s.Store.InTx(ctx, buildLock(bb), func(tx *store.Tx) error {
				if err := tx.PublicationLive(ctx, bb.Source, bb.PublisherKeyID); err != nil {
					return err
				}
				return tx.FindOrInsertBuild(ctx, &bb)
			})
			if err == nil {
				it.b = bb
				break
			}
			if !errors.Is(err, store.ErrExists) || try > 0 {
				return nil, false, slotErr(it.origin, err) // spec 0033: the source's slot holds another body
			}
		}
		if s.afterBuild != nil {
			s.afterBuild(it.b)
		}
		rs, existed, err := ins(ctx, *it)
		if errors.Is(err, store.ErrBuildGone) && attempt == 0 {
			continue
		}
		return rs, existed, slotErr(it.origin, err)
	}
}

// buildLock is the lock a build's transaction takes: a publisher's publication takes the tenant's
// auth lock, which removing its key or the publisher takes too, so an upload in flight when they are
// removed is refused (store.PublicationLive) instead of landing after the removal's purge of held
// entries (spec 0033 phase A2); other arrivals take none.
func buildLock(b store.Build) string {
	if strings.HasPrefix(b.Source, store.SourcePublisherPrefix) {
		return store.PublisherAuthLock(b.TenantID)
	}
	return ""
}

// land commits a spooled body and finds or inserts its Build, with the keys a trusted intake
// carried, placing it nowhere (spec 0033: a catalogue entry), under the intake deadline. event, when
// set, writes the arrival's event in the same transaction (inserted: the build is new). Two intakes
// of one body race on the insert: the loser finds the winner's build.
func (s *Service) land(ctx context.Context, sp *blob.Spool, it *item, event func(tx *store.Tx, b store.Build, inserted bool) error) error {
	ctx, cancel := context.WithTimeout(ctx, blob.IntakeDeadline)
	defer cancel()
	if _, err := sp.Commit(ctx); err != nil {
		return err
	}
	for try := 0; ; try++ {
		bb := it.b
		bb.Visibility, bb.Provenance = it.visibility, it.provenance
		err := s.Store.InTx(ctx, buildLock(bb), func(tx *store.Tx) error {
			if err := tx.PublicationLive(ctx, bb.Source, bb.PublisherKeyID); err != nil {
				return err
			}
			if err := tx.FindOrInsertBuild(ctx, &bb); err != nil {
				return err
			}
			if err := tx.SetLanded(ctx, bb.ID); err != nil {
				return err
			}
			if err := recordKeys(ctx, tx, bb, *it); err != nil {
				return err
			}
			if event != nil {
				return event(tx, bb, bb.Inserted)
			}
			return nil
		})
		if errors.Is(err, store.ErrExists) && try == 0 {
			continue
		}
		if err == nil {
			it.b = bb
		}
		return err
	}
}

// Purge deletes a yanked release of a channel for good (spec 0016 phase 2): its signatures go, its
// slot stays taken (never refilled, its sequence number never given again), and its Build loses the
// reference, so the storage collector removes the body later. With a name, the release must be of
// that extension; a non-zero expected version must be the release's. It returns what was purged.
func (s *Service) Purge(ctx context.Context, a authz.Actor, tenant, channel, name, id string, expected int64) (store.Candidate, error) {
	if err := s.Authz.Allow(ctx, a, authz.VerbAdmin, authz.Resource{Tenant: tenant, Channel: channel, Package: name}); err != nil {
		return store.Candidate{}, err
	}
	if s.PurgeOff {
		return store.Candidate{}, fmt.Errorf("%w: purging is off until the server administrator sets gc.interval "+
			"(every replica then runs a version that keeps purged slots)", ErrState)
	}
	ch, err := s.Store.GetChannel(ctx, tenant, channel)
	if err != nil {
		return store.Candidate{}, err
	}
	var out store.Candidate
	err = s.Store.InTenantTx(ctx, ch.TenantID, []string{lockKey(ch.ID)}, func(tx *store.Tx) error {
		c, err := tx.GetCandidate(ctx, ch.ID, id)
		if err != nil {
			return err
		}
		if name != "" && c.Name != name {
			return fmt.Errorf("%w: release %s", store.ErrNotFound, id) // another extension's id
		}
		if expected != 0 && c.Version != expected {
			return store.ErrConflict
		}
		if c.State != store.ReleaseYanked {
			return fmt.Errorf("%w: only a yanked release is purged; yank it first", ErrState)
		}
		if err := tx.PurgeRelease(ctx, c); err != nil {
			return err
		}
		out = c
		if err := tx.Event(ctx, ch.TenantID, a.String(), "release.purge", releaseSubject(ch, c.Release),
			map[string]any{"release": c.ID, "name": c.Name, "version": c.PackageVersion, "platform": c.Platform, "slot": c.Slot,
				"body_hash": c.BodyHash}); err != nil {
			return err
		}
		return tx.BumpReleaseVersion(ctx, ch.ID)
	})
	return out, err
}

// anyTakesNow is AnyTakes inside a release's transaction, under its locks: the server's list as it is
// now, and no name the tenant reserves or an upstream of the tenant lists (spec 0030).
func anyTakesNow(ctx context.Context, tx *store.Tx, tenantID, packageType, name, kind string) (bool, error) {
	k, err := tx.ReservedKind(ctx, packageType, name)
	if err != nil || k == store.ReservedCore || k == store.ReservedCommunity && kind != store.UpstreamCommunity {
		return false, err
	}
	if held, err := tx.TenantReserves(ctx, tenantID, packageType, name); err != nil || held {
		return false, err
	}
	listed, err := tx.UpstreamProvides(ctx, tenantID, packageType, name)
	return !listed, err
}
