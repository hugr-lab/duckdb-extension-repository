package serve

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// NamesPage is the most names a page of -/names answers (spec 0039).
const NamesPage = 500

// NamesAnswer is a page of -/names.
type NamesAnswer struct {
	API   int      `json:"api"`
	Names []string `json:"names"`
	Next  string   `json:"next,omitempty"`
}

// names answers GET <prefix>/-/names[/<cursor>] (spec 0039 PR 2): the extension names the channel
// serves in the caller's view (anonymous: those with a public release; a token: also those it holds
// install on), 500 a page, so a "*" mirror of the channel learns them. A suspended or deleting
// tenant, a missing or a passthrough channel answer as a missing path.
func (h *Handler) names(w http.ResponseWriter, r *http.Request, rt route) {
	ctx := r.Context()
	sc, err := h.st.GetServeChannel(ctx, rt.tenant, rt.channel)
	if err != nil || sc.Tenant.State != store.TenantActive {
		h.storeError(w, err)
		return
	}
	if sc.Channel.Kind != store.ChannelSigned {
		notFound(w)
		return
	}
	installs, actor, r := h.identify(r, sc, rt.kind.String())
	snap, err := h.rv.snaps.Get(ctx, sc.Channel)
	if err != nil {
		h.fail(w, err)
		return
	}
	x := snap.NamesIndex()
	sum := sha256.New()
	sum.Write(x.Digest)
	sum.Write([]byte(rt.cursor + "\x00"))
	view := func(n string) bool { return x.Public[n] }
	if actor != "" {
		held := map[string]bool{}
		for _, n := range x.All {
			if installs(n) {
				held[n] = true
				sum.Write([]byte(n + "\x00"))
			}
		}
		view = func(n string) bool { return held[n] || x.Public[n] }
	}
	etag := `"` + hex.EncodeToString(sum.Sum(nil)[:16]) + `"`
	start := 0
	if rt.cursor != "" {
		after, err := base64.RawURLEncoding.DecodeString(rt.cursor)
		if err != nil {
			notFound(w)
			return
		}
		start = sort.SearchStrings(x.All, string(after)+"\x00")
	}
	out := NamesAnswer{API: 1, Names: []string{}}
	for _, n := range x.All[start:] {
		if !view(n) {
			continue
		}
		if len(out.Names) == NamesPage {
			out.Next = base64.RawURLEncoding.EncodeToString([]byte(out.Names[len(out.Names)-1]))
			break
		}
		out.Names = append(out.Names, n)
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
