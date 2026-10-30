package serve

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

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

// revocations answers GET <prefix>/-/revocations[/<cursor>] (spec 0039): the channel's revoked rows
// in the caller's view (anonymous: public releases; a token: also those of the names it holds
// install on), 1,000 a page. A suspended tenant answers tokens holding install only, with the names
// they hold it on (Enterest withdraws in suspended tenants); a deleting tenant, a missing or a
// passthrough channel answer as a missing path.
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
	vis := func(c *store.Candidate) bool {
		return installs(c.Name) || !suspended && c.Visibility == store.Public
	}
	rows := snap.RevokedRows(vis)
	// the ETag is over every row of the caller's view, so a change on any page, or of the view (a
	// grant), is never answered 304
	sum := sha256.New()
	for _, x := range rows {
		b, _ := json.Marshal(x)
		sum.Write(b)
		sum.Write([]byte{'\n'})
	}
	sum.Write([]byte(rt.cursor))
	etag := `"` + hex.EncodeToString(sum.Sum(nil)[:16]) + `"`
	start := 0
	if rt.cursor != "" {
		after, ok := decodeCursor(rt.cursor)
		if !ok {
			notFound(w)
			return
		}
		for start < len(rows) && !keyAfter(rows[start].Key(), after) {
			start++
		}
	}
	end := min(start+RevocationsPage, len(rows))
	out := RevocationsAnswer{API: 1, Rows: rows[start:end]}
	if out.Rows == nil {
		out.Rows = []release.RevokedRow{}
	}
	if end < len(rows) {
		out.Next = encodeCursor(rows[end-1].Key())
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
		for i := range list {
			if installs(list[i].Name) {
				return true
			}
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
