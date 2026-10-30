package serve

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hugr-lab/duckdb-extension-repository/internal/release"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// RevocationsPage is the most rows a page of -/revocations answers (spec 0039).
const RevocationsPage = 1000

// RevocationsAnswer is a page of -/revocations: its shape is the probe a downstream recognises a
// kista by.
type RevocationsAnswer struct {
	API  int                  `json:"api"`
	Rows []release.RevokedRow `json:"rows"`
	Next string               `json:"next,omitempty"`
}

// revokedRows are a snapshot's -/revocations rows, computed once per snapshot: every row (the view
// of a caller holding install on its name) and the public view's, both in key order. A view's rows
// are a function of the two and of the names it holds install on, so its ETag is derived from the
// digest of both and those names, without rebuilding the rows.
type revokedRows struct {
	snap   *release.Snapshot
	all    []release.RevokedRow
	public map[[4]string]release.RevokedRow
	names  []string // the names of all's rows, once each
	digest []byte
}

// revocationsCache keeps each channel's rows of its current snapshot.
type revocationsCache struct {
	mu sync.Mutex
	by map[string]*revokedRows
}

func (c *revocationsCache) get(channelID string, snap *release.Snapshot) *revokedRows {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.by[channelID]; r != nil && r.snap == snap {
		return r
	}
	r := &revokedRows{snap: snap, all: snap.RevokedRows(release.Everything), public: map[[4]string]release.RevokedRow{}}
	sum := sha256.New()
	for _, x := range r.all {
		if len(r.names) == 0 || r.names[len(r.names)-1] != x.Name {
			r.names = append(r.names, x.Name)
		}
		b, _ := json.Marshal(x)
		sum.Write(b)
	}
	sum.Write([]byte{0})
	for _, x := range snap.RevokedRows(func(c *store.Candidate) bool { return c.Visibility == store.Public }) {
		r.public[x.Key()] = x
		b, _ := json.Marshal(x)
		sum.Write(b)
	}
	r.digest = sum.Sum(nil)
	if c.by == nil {
		c.by = map[string]*revokedRows{}
	}
	c.by[channelID] = r
	return r
}

// revView is the rows a caller sees: of the names it holds install on, every row; of the others, the
// public view's row (none in a suspended tenant).
type revView struct {
	rows      *revokedRows
	held      map[string]bool
	suspended bool
}

func (v revView) row(i int) (release.RevokedRow, bool) {
	x := v.rows.all[i]
	if v.held[x.Name] {
		return x, true
	}
	if v.suspended {
		return release.RevokedRow{}, false
	}
	p, ok := v.rows.public[x.Key()]
	return p, ok
}

// revocations answers GET <prefix>/-/revocations[/<cursor>] (spec 0039): the channel's revoked rows
// in the caller's view (anonymous: public releases; a token: also those of the names it holds
// install on), 1,000 a page. A suspended tenant answers tokens holding install on a name of the
// channel only, with the names they hold it on (Enterest withdraws in suspended tenants); a deleting
// tenant, a missing or a passthrough channel answer as a missing path.
func (h *Handler) revocations(w http.ResponseWriter, r *http.Request, rt route) {
	ctx := r.Context()
	sc, err := h.st.GetServeChannel(ctx, rt.tenant, rt.channel)
	if err != nil || sc.Tenant.State != store.TenantActive && sc.Tenant.State != store.TenantSuspended {
		h.storeError(w, err)
		return
	}
	if sc.Channel.Kind != store.ChannelSigned {
		notFound(w)
		return
	}
	installs, actor, r := h.identify(r, sc, rt.kind.String())
	suspended := sc.Tenant.State == store.TenantSuspended
	if suspended && actor == "" {
		notFound(w)
		return
	}
	snap, err := h.rv.snaps.Get(ctx, sc.Channel)
	if err != nil {
		h.fail(w, err)
		return
	}
	if suspended && !holdsAny(snap, installs) { // a token holding install on none of the channel's names
		notFound(w)
		return
	}
	rows := h.revs.get(sc.Channel.ID, snap)
	v := revView{rows: rows, held: map[string]bool{}, suspended: suspended}
	sum := sha256.New()
	sum.Write(rows.digest)
	sum.Write([]byte(strconv.FormatBool(suspended) + "\x00" + rt.cursor + "\x00"))
	if actor != "" {
		for _, n := range rows.names {
			if installs(n) {
				v.held[n] = true
				sum.Write([]byte(n + "\x00"))
			}
		}
	}
	etag := `"` + hex.EncodeToString(sum.Sum(nil)[:16]) + `"`
	start := 0
	if rt.cursor != "" {
		after, ok := decodeCursor(rt.cursor)
		if !ok {
			notFound(w)
			return
		}
		start = sort.Search(len(rows.all), func(i int) bool { return keyAfter(rows.all[i].Key(), after) })
	}
	out := RevocationsAnswer{API: 1, Rows: []release.RevokedRow{}}
	for i := start; i < len(rows.all); i++ {
		x, ok := v.row(i)
		if !ok {
			continue
		}
		if len(out.Rows) == RevocationsPage {
			out.Next = encodeCursor(out.Rows[len(out.Rows)-1].Key())
			break
		}
		out.Rows = append(out.Rows, x)
	}
	hd := w.Header()
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Cache-Control", "private, no-cache")
	hd.Set("Vary", "Authorization")
	hd.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	b, _ := json.Marshal(out)
	hd.Set("Content-Type", "application/json")
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(b)
	}
}

// holdsAny reports whether installs holds a name the channel has a release of, live or revoked.
func holdsAny(snap *release.Snapshot, installs func(string) bool) bool {
	for _, list := range [][]store.Candidate{snap.Releases, snap.Revoked} {
		if slices.ContainsFunc(list, func(c store.Candidate) bool { return installs(c.Name) }) {
			return true
		}
	}
	return false
}

// encodeCursor is a page cursor: the last row's key, base64url.
func encodeCursor(k [4]string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strings.Join(k[:], "\x00")))
}

func decodeCursor(s string) ([4]string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return [4]string{}, false
	}
	parts := strings.Split(string(b), "\x00")
	if len(parts) != 4 {
		return [4]string{}, false
	}
	return [4]string{parts[0], parts[1], parts[2], parts[3]}, true
}

func keyAfter(k, after [4]string) bool {
	for i := range k {
		if k[i] != after[i] {
			return k[i] > after[i]
		}
	}
	return false
}
