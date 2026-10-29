package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/authz"
	"github.com/hugr-lab/duckdb-extension-repository/internal/pkgtype"
	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Spec 0033: a tenant's catalogue, and placing its builds into a channel by hand.

type catalogueEntryJSON struct {
	PackageType string            `json:"package_type,omitempty"` // spec 0034: omitted when it is the default
	Source      string            `json:"source"`
	Name        string            `json:"name"`
	Versions    []string          `json:"versions"` // newest first
	Rules       map[string]string `json:"rules"`    // the channels whose rule takes the source, with its mode
	// Held are the versions with a held build (spec 0033 phase A2), newest first; omitted for none
	Held []string `json:"held,omitempty"`
	// Revoked are the versions with a revoked build (spec 0033 phase A2: kept by a purge, placed
	// automatically nowhere), newest first; omitted for none
	Revoked []string `json:"revoked,omitempty"`
	// Withdrawn are the versions with a withdrawn build (spec 0033 phase B: served nowhere), newest
	// first; omitted for none
	Withdrawn []string `json:"withdrawn,omitempty"`
	// PublisherID is a distributor's publisher's id, to a distribution admin, a server administrator
	// and the publisher itself (spec 0033 phase B: a name two of them had is publisher-id:<id> as a
	// source; spec 0037: its signed read); omitted otherwise
	PublisherID string `json:"publisher_id,omitempty"`
	// Distributor names a distributor's publisher's entry's distributor, as the publishers' answers
	// do; Summary and Icon are the entry's metadata (spec 0036), omitted for none
	Distributor string    `json:"distributor,omitempty"`
	Summary     string    `json:"summary,omitempty"`
	Icon        *iconJSON `json:"icon,omitempty"`
}

func (h *Handler) catalogue(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	q := r.URL.Query()
	es, err := h.o.Releases.Catalogue(r.Context(), a, p["t"], reqType(r), q.Get("source"), q.Get("name"))
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	out := []catalogueEntryJSON{}
	for _, e := range es {
		out = append(out, catalogueEntryJSON{PackageType: shownType(reqType(r)), Source: e.Source, Name: e.Name, Versions: e.Versions, Rules: e.Rules,
			Held: e.Held, Revoked: e.Revoked, Withdrawn: e.Withdrawn, PublisherID: e.PublisherID, Distributor: e.Distributor,
			Summary: e.Meta.Summary, Icon: iconOf(e.Meta)})
	}
	reply(w, r, http.StatusOK, map[string]any{"entries": out}, "", noStore)
}

type catalogueBuildJSON struct {
	PackageType   string `json:"package_type,omitempty"` // spec 0034: omitted when it is the default
	ID            string `json:"id"`
	Platform      string `json:"platform"`
	Slot          string `json:"slot"`
	ABI           string `json:"abi"`
	DuckDBVersion string `json:"duckdb_version,omitempty"`
	CAPI          string `json:"c_api,omitempty"`
	BodyHash      string `json:"body_hash"`
	Visibility    string `json:"visibility"`
	Origin        string `json:"origin"`
	Checked       bool   `json:"checked"`
	CreatedAt     string `json:"created_at"`
	// State is held, available or withdrawn (spec 0033 phases A2 and B); AvailableAt, a held build's
	// end of hold
	State       string `json:"state"`
	AvailableAt string `json:"available_at,omitempty"`
	// RevokedAt: when a purge kept it, revoked (spec 0033 phase A2: its publisher key revoked with
	// purge_held, or its publisher removed, while it was held and placed early); omitted for none
	RevokedAt string `json:"revoked_at,omitempty"`
	// WithdrawnAt and WithdrawnReason: when and why it was withdrawn (spec 0033 phase B); omitted for
	// none
	WithdrawnAt     string `json:"withdrawn_at,omitempty"`
	WithdrawnReason string `json:"withdrawn_reason,omitempty"`
	// Placed is where it is released; omitted for a distribution admin, who reads no channel (spec
	// 0033 phase B)
	Placed *[]store.Placed `json:"placed,omitempty"`
	// PublisherID and Distributor name the publisher that published, in a catalogue-only
	// publication's answer (spec 0037); Distributor is omitted for the tenant's own publisher, both
	// for a tenant principal's publication
	PublisherID string `json:"publisher_id,omitempty"`
	Distributor string `json:"distributor,omitempty"`
}

func (h *Handler) catalogueEntry(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	vs, err := h.o.Releases.CatalogueVersions(r.Context(), a, p["t"], reqType(r), p["source"], p["ext"])
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	type versionJSON struct {
		Version string               `json:"version"`
		Builds  []catalogueBuildJSON `json:"builds"`
	}
	out := []versionJSON{}
	for _, v := range vs {
		vj := versionJSON{Version: v.Version, Builds: []catalogueBuildJSON{}}
		for _, b := range v.Builds {
			bj := catalogueBuildOf(b.Build, b.Placed, h.o.Store.Now())
			if c.distribution && !c.admin || c.pub != nil { // spec 0037: a distributor's publisher reads no channel either
				bj.Placed = nil
			}
			vj.Builds = append(vj.Builds, bj)
		}
		out = append(out, vj)
	}
	ans := map[string]any{"source": p["source"], "name": p["ext"], "versions": out}
	if t := shownType(reqType(r)); t != "" {
		ans["package_type"] = t
	}
	// spec 0036: the entry's metadata and distributor, when the caller reads its metadata (every
	// catalogue reader does, but a distributor's publisher on a name reserved server-wide since)
	switch m, err := h.o.Releases.EntryMeta(r.Context(), a, p["t"], reqType(r), p["source"], p["ext"]); {
	case err == nil:
		if m.Summary != "" {
			ans["summary"] = m.Summary
		}
		if ic := iconOf(m.EntryMeta); ic != nil {
			ans["icon"] = ic
		}
		if m.Distributor != "" {
			ans["distributor"] = m.Distributor
		}
	case !errors.Is(err, authz.ErrDenied) && !errors.Is(err, store.ErrNotFound):
		h.fail(w, err)
		return
	}
	reply(w, r, http.StatusOK, ans, "", noStore)
}

func (h *Handler) placeRelease(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Source     string `json:"source"`
		Version    string `json:"version"`
		Visibility string `json:"visibility"`
		Current    *bool  `json:"current"`
		// PackageType is the build's type ("": the channel's); another type than the channel's is
		// refused (spec 0034: 409)
		PackageType string `json:"package_type"`
		// Early places a held entry (spec 0033 phase A2): a channel administrator's, audited
		Early bool `json:"early"`
	}
	if !body(w, r, &in, maxBody) {
		return
	}
	if in.PackageType != "" && !pkgtype.Known(in.PackageType) {
		problem(w, http.StatusBadRequest, typeInvalid, "package_type is a package type this server knows")
		return
	}
	o := release.PlaceOptions{Source: in.Source, Version: in.Version, PackageType: in.PackageType, Early: in.Early}
	switch in.Visibility {
	case "":
	case store.Private:
		o.Private = true
	default:
		problem(w, http.StatusBadRequest, typeInvalid, "visibility can only be narrowed to private; a placement keeps the build's")
		return
	}
	o.NotCurrent = in.Current != nil && !*in.Current
	a, _ := c.actor()
	rels, existed, err := h.o.Releases.Place(r.Context(), a, p["t"], p["c"], p["ext"], o)
	if err != nil {
		h.publishErr(w, err, false)
		return
	}
	h.answerReleases(w, r, c, p, rels, existed, false)
}

// publishToCatalogue publishes into the tenant's catalogue only (spec 0033): publish on the name
// tenant-wide, the same request as a channel's publication without "current".
func (h *Handler) publishToCatalogue(w http.ResponseWriter, r *http.Request, c caller, p params) {
	u, ok := h.readUpload(w, r, c, false)
	if !ok {
		return
	}
	defer u.done()
	a, _ := c.actor()
	b, existed, err := h.o.Releases.AddToCatalogue(r.Context(), a, p["t"], u.body, release.AddOptions{
		Name: p["ext"], PackageType: reqType(r), Private: u.private, NoWait: true,
		Publish: &release.Publication{Version: u.version, Platform: u.platform, Provenance: provenance(a, c), KeySources: keySources(c),
			Statement: c.statement(), Signature: c.signature(), KeyID: keyID(c)},
	})
	if err != nil {
		h.publishErr(w, err, false)
		return
	}
	code := http.StatusCreated
	if existed {
		code = http.StatusOK
	}
	bj := catalogueBuildOf(b, nil, h.o.Store.Now())
	dist, err := h.o.Store.SourceDistributor(r.Context(), b.TenantID, b.Source)
	if err != nil || dist != "" {
		bj.Placed = nil // a distributor's publisher reaches no channel, and learns nothing of them (spec 0033 phase B)
	}
	if id, ok := strings.CutPrefix(b.Source, store.SourcePublisherPrefix); ok && err == nil {
		bj.PublisherID, bj.Distributor = id, dist // spec 0037: the publisher that published
	}
	reply(w, r, code, bj, "", noStore)
}

// withdraw withdraws a version of a name from a distributor's publisher's source in the tenant (spec
// 0033 phase B): POST …/catalogue/{source}/extensions/{ext}/versions/{v}/withdraw {"reason": …}, a
// distribution admin's or a server administrator's. The answer names the tenant, the version and the
// entries withdrawn, never the tenant's channels.
func (h *Handler) withdraw(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !body(w, r, &in, maxBody) {
		return
	}
	a, _ := c.actor()
	wd, err := h.o.Releases.Withdraw(r.Context(), a, p["t"], reqType(r), p["source"], p["ext"], p["v"], in.Reason)
	if err != nil {
		h.serviceErr(w, err, false)
		return
	}
	type entryJSON struct {
		Build    string `json:"build"`
		Platform string `json:"platform"`
		Slot     string `json:"slot"`
	}
	out := struct {
		PackageType string      `json:"package_type,omitempty"`
		Tenant      string      `json:"tenant"`
		Source      string      `json:"source"`
		Name        string      `json:"name"`
		Version     string      `json:"version"`
		Reason      string      `json:"reason"`
		Withdrawn   []entryJSON `json:"withdrawn"` // the entries this withdrawal withdrew
		Already     bool        `json:"already"`   // the version was withdrawn already
	}{PackageType: shownType(wd.PackageType), Tenant: wd.Tenant, Source: wd.Source, Name: wd.Name, Version: wd.Version, Reason: wd.Reason,
		Withdrawn: []entryJSON{}, Already: wd.Already}
	for _, b := range wd.Builds {
		out.Withdrawn = append(out.Withdrawn, entryJSON{Build: b.ID, Platform: b.Platform, Slot: b.Slot()})
	}
	reply(w, r, http.StatusOK, out, "", noStore)
}

func (h *Handler) purgeEntry(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if _, err := h.o.Releases.PurgeEntry(r.Context(), a, p["t"], p["id"]); err != nil {
		h.serviceErr(w, err, false)
		return
	}
	noContent(w)
}

func catalogueBuildOf(b store.Build, placed []store.Placed, now time.Time) catalogueBuildJSON {
	bj := catalogueBuildJSON{PackageType: shownType(b.PackageType), ID: b.ID, Platform: b.Platform, Slot: b.Slot(), ABI: b.ABI, DuckDBVersion: b.DuckDBVersion,
		BodyHash: b.BodyHash, Visibility: b.Visibility, Origin: b.Origin, Checked: b.Checked, CreatedAt: timeOf(b.CreatedAt), Placed: &placed,
		State: "available"}
	if b.Held(now) {
		bj.State, bj.AvailableAt = "held", timeOf(b.AvailableAt)
	}
	if b.Revoked() {
		bj.RevokedAt = timeOf(b.RevokedAt)
	}
	if b.Withdrawn() {
		bj.State, bj.WithdrawnAt, bj.WithdrawnReason = "withdrawn", timeOf(b.WithdrawnAt), b.WithdrawnReason
	}
	if b.CAPI != nil {
		bj.CAPI = b.CAPI.String()
	}
	if placed == nil {
		placed = []store.Placed{}
	}
	return bj
}
