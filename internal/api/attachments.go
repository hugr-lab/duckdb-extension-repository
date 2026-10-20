package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hugr-lab/duckdb-extension-repository/internal/attachment"
	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Attachments (spec 0011 phase 1a): readers see the readers-visible chain heads through the index,
// in their view; administrators (and the tenant's auditors) manage every document under the
// extension.

type attachmentJSON struct {
	ID         string          `json:"id"`
	BodyHash   string          `json:"body_hash"`
	Kind       string          `json:"kind"`
	MediaType  string          `json:"media_type"`
	Title      string          `json:"title"`
	Size       int64           `json:"size"`
	Summary    json.RawMessage `json:"summary"`
	Origin     string          `json:"origin"`
	Issuer     string          `json:"issuer,omitempty"` // a feed's document: its source, whose key signed it
	IssuedAt   string          `json:"issued_at"`
	ValidUntil string          `json:"valid_until,omitempty"`
	Expired    bool            `json:"expired,omitempty"`
	// management only: who attached it, what it supersedes, its visibility and version
	AttachedBy string `json:"attached_by,omitempty"`
	Supersedes string `json:"supersedes,omitempty"`
	Head       *bool  `json:"head,omitempty"`
	Visibility string `json:"visibility,omitempty"`
	Feedable   *bool  `json:"feedable,omitempty"`
	Withdrawn  bool   `json:"withdrawn,omitempty"`
	// a feed's document: who stands behind it at its source, and the key that signed it
	SourceOrigin string `json:"source_origin,omitempty"`
	Key          string `json:"key,omitempty"`
	Audience     string `json:"audience,omitempty"` // who its source lets subscribers show it to
	ETag         string `json:"etag,omitempty"`
}

func (h *Handler) attachmentOf(a store.Attachment, manage bool, head bool, c caller) attachmentJSON {
	o := attachmentJSON{ID: a.ID, BodyHash: a.BodyHash, Kind: a.Kind, MediaType: a.MediaType, Title: a.Title, Size: a.Size,
		Summary: json.RawMessage(a.Summary), Origin: a.Origin, Issuer: a.Issuer, IssuedAt: timeOf(a.IssuedAt), ValidUntil: timeOf(a.ValidUntil),
		Expired: attachment.Expired(a, h.now())}
	if manage {
		o.AttachedBy, o.Supersedes, o.Visibility, o.ETag = createdBy(a.AttachedBy, c), a.Supersedes, a.Visibility, versionTag(a.Version)
		o.Head, o.Feedable, o.Withdrawn = &head, &a.Feedable, !a.WithdrawnAt.IsZero()
		o.SourceOrigin, o.Key = a.SourceOrigin, a.KeyFP
		if a.Origin == store.AttachedByFeed {
			o.Audience = attachment.Audience(a)
		}
	}
	return o
}

func (h *Handler) now() time.Time {
	if h.o.Now != nil {
		return h.o.Now()
	}
	return time.Now()
}

// pagedAttachments filters by body hash and kind, and pages by (body hash, kind, created, id).
func pagedAttachments(w http.ResponseWriter, r *http.Request, as []store.Attachment) ([]store.Attachment, string, bool) {
	limit, after, ok := page(w, r)
	if !ok {
		return nil, "", false
	}
	q := r.URL.Query()
	body, kind := q.Get("body_hash"), q.Get("kind")
	if kind != "" && !slices.Contains(attachment.Kinds, kind) {
		problem(w, http.StatusBadRequest, typeInvalid, "kind is one of "+strings.Join(attachment.Kinds, ", "))
		return nil, "", false
	}
	as = slices.DeleteFunc(slices.Clone(as), func(a store.Attachment) bool {
		return body != "" && a.BodyHash != body || kind != "" && a.Kind != kind
	})
	key := func(a store.Attachment) string {
		return a.BodyHash + "|" + a.Kind + "|" + a.CreatedAt.UTC().Format("20060102T150405.000000000") + "|" + a.ID
	}
	out, next := paged(as, key, limit, after)
	return out, next, true
}

// readableAttachments are the documents a caller may read through the index: the readers-visible
// heads of the extension's bodies in its view of the channel.
func (h *Handler) readableAttachments(w http.ResponseWriter, r *http.Request, c caller, p params) ([]store.Attachment, bool) {
	if h.o.Attachments == nil {
		notFound(w)
		return nil, false
	}
	snap, ok := h.snapshot(w, r, c, p)
	if !ok {
		return nil, false
	}
	rs, ok := h.indexResolver(w, r, c, snap, p["name"])
	if !ok {
		return nil, false
	}
	bodies := map[string]bool{}
	for _, o := range rs.rowsOf(p["name"], "") {
		bodies[o.BodyHash] = true
	}
	all, err := h.o.Store.ExtensionAttachments(r.Context(), c.tenant.ID, p["name"])
	if err != nil {
		h.fail(w, err)
		return nil, false
	}
	return attachment.Readable(all, bodies), true
}

func (h *Handler) indexAttachments(w http.ResponseWriter, r *http.Request, c caller, p params) {
	as, ok := h.readableAttachments(w, r, c, p)
	if !ok {
		return
	}
	as, next, ok := pagedAttachments(w, r, as)
	if !ok {
		return
	}
	out := []attachmentJSON{}
	for _, a := range as {
		out = append(out, h.attachmentOf(a, false, true, c))
	}
	answer(w, r, list("attachments", out, next), c.anonymous())
}

func (h *Handler) indexAttachment(w http.ResponseWriter, r *http.Request, c caller, p params) {
	as, ok := h.readableAttachments(w, r, c, p)
	if !ok {
		return
	}
	for _, a := range as {
		if a.ID == p["id"] {
			if err := h.o.Attachments.ReadContent(r.Context(), c.tenant, a, func(b []byte) error { download(w, a, b); return nil }); err != nil {
				h.fail(w, err)
			}
			return
		}
	}
	notFound(w)
}

// fileExt is a download's file extension by media type.
func fileExt(mt string) string {
	if mt == attachment.Markdown {
		return "md"
	}
	return "json"
}

// downloadDeadline bounds sending a document (16 MiB at least 140 KB/s).
const downloadDeadline = 2 * time.Minute

// download sends a document as a file, never to be rendered (spec 0011): the stored canonical media
// type, a fixed file name, nosniff, a sandbox, and no caching (its visibility can change).
func download(w http.ResponseWriter, a store.Attachment, b []byte) {
	hd := w.Header()
	hd.Set("Content-Type", a.MediaType)
	hd.Set("Content-Disposition", `attachment; filename="`+a.ID+"."+fileExt(a.MediaType)+`"`)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "sandbox")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("Content-Length", strconv.Itoa(len(b)))
	// the content holds one of the read slots while it is sent: a slow reader does not keep it long
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadDeadline))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// --- management ---

func (h *Handler) attachmentErr(w http.ResponseWriter, err error) {
	if errors.Is(err, attachment.ErrDocument) {
		problem(w, http.StatusBadRequest, typeInvalid, publicMessage(err))
		return
	}
	h.serviceErr(w, err, false)
}

func (h *Handler) listAttachments(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	all, err := h.o.Attachments.List(r.Context(), a, p["t"], p["ext"])
	if err != nil {
		h.attachmentErr(w, err)
		return
	}
	heads := map[string]bool{}
	for _, x := range attachment.Heads(all) {
		heads[x.ID] = true
	}
	if o := r.URL.Query().Get("origin"); o != "" {
		all = slices.DeleteFunc(all, func(x store.Attachment) bool { return x.Origin != o })
	}
	as, next, ok := pagedAttachments(w, r, all)
	if !ok {
		return
	}
	out := []attachmentJSON{}
	for _, x := range as {
		out = append(out, h.attachmentOf(x, true, heads[x.ID], c))
	}
	reply(w, r, http.StatusOK, list("attachments", out, next), "", noStore)
}

func (h *Handler) getAttachment(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	x, err := h.o.Attachments.Get(r.Context(), a, p["t"], p["ext"], p["id"])
	if err != nil {
		h.attachmentErr(w, err)
		return
	}
	all, err := h.o.Store.ExtensionAttachments(r.Context(), c.tenant.ID, p["ext"])
	if err != nil {
		h.fail(w, err)
		return
	}
	head := slices.ContainsFunc(attachment.Heads(all), func(y store.Attachment) bool { return y.ID == x.ID })
	reply(w, r, http.StatusOK, h.attachmentOf(x, true, head, c), versionTag(x.Version), noStore)
}

func (h *Handler) attachmentContent(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	err := h.o.Attachments.Content(r.Context(), a, p["t"], p["ext"], p["id"], func(x store.Attachment, b []byte) error {
		download(w, x, b)
		return nil
	})
	if err != nil {
		h.attachmentErr(w, err)
	}
}

var attachParams = map[string]bool{"body_hash": true, "kind": true, "supersedes": true}

func (h *Handler) addAttachment(w http.ResponseWriter, r *http.Request, c caller, p params) {
	q := r.URL.Query()
	for k, vs := range q {
		if !attachParams[k] || len(vs) != 1 {
			problem(w, http.StatusBadRequest, typeInvalid, "the parameters are body_hash, kind and supersedes, once each")
			return
		}
	}
	if r.ContentLength < 0 || len(r.TransferEncoding) > 0 {
		problem(w, http.StatusLengthRequired, typeInvalid, "Content-Length is required")
		return
	}
	max := h.o.Attachments.MaxSize
	if max <= 0 {
		max = attachment.DefaultMaxSize
	}
	if r.ContentLength > max {
		problem(w, http.StatusRequestEntityTooLarge, typeTooLarge, "the document is larger than this server accepts")
		return
	}
	// the title: percent-encoded UTF-8 (a header carries no other)
	title, err := url.PathUnescape(r.Header.Get("Kista-Title"))
	if err != nil {
		problem(w, http.StatusBadRequest, typeInvalid, "Kista-Title is percent-encoded UTF-8")
		return
	}
	a, _ := c.actor()
	// a document is held in memory and parsed: attaching takes an upload slot, as publishing does
	done, ok := h.slot(w, a.String(), p["t"])
	if !ok {
		return
	}
	defer done()
	b, err := io.ReadAll(io.LimitReader(r.Body, r.ContentLength))
	if errors.Is(err, errBodyNotSigned) {
		problem(w, http.StatusBadRequest, typeInvalid, "the body is not the signed one: its SHA-256 is not the statement's")
		return
	}
	if err != nil || int64(len(b)) != r.ContentLength {
		problem(w, http.StatusBadRequest, typeInvalid, "the body ended before its Content-Length")
		return
	}
	x, err := h.o.Attachments.Attach(r.Context(), a, p["t"], p["ext"], attachment.AttachInput{BodyHash: q.Get("body_hash"),
		Kind: q.Get("kind"), MediaType: r.Header.Get("Content-Type"), Title: title, Supersedes: q.Get("supersedes"),
		Content: b})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			problem(w, http.StatusBadRequest, typeInvalid, "the body is not one of this extension's, or the document it supersedes does not exist")
			return
		}
		h.attachmentErr(w, err)
		return
	}
	created(w, r, "/api/v1/tenants/"+esc(p["t"])+"/extensions/"+esc(p["ext"])+"/attachments/"+esc(x.ID), h.attachmentOf(x, true, true, c),
		versionTag(x.Version))
}

func (h *Handler) changeAttachment(w http.ResponseWriter, r *http.Request, c caller, p params) {
	var vis *string
	var feed *bool
	switch p["change"] {
	case store.VisibleReaders, store.VisibleAdministrators:
		v := p["change"]
		vis = &v
	case "feedable", "unfeedable":
		f := p["change"] == "feedable"
		feed = &f
	default:
		notFound(w)
		return
	}
	expected, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	a, _ := c.actor()
	x, err := h.o.Attachments.SetFlags(r.Context(), a, p["t"], p["ext"], p["id"], vis, feed, expected)
	if err != nil {
		h.attachmentErr(w, err)
		return
	}
	all, err := h.o.Store.ExtensionAttachments(r.Context(), c.tenant.ID, p["ext"])
	if err != nil {
		h.fail(w, err)
		return
	}
	head := slices.ContainsFunc(attachment.Heads(all), func(y store.Attachment) bool { return y.ID == x.ID })
	reply(w, r, http.StatusOK, h.attachmentOf(x, true, head, c), versionTag(x.Version), noStore)
}

func (h *Handler) removeAttachment(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Attachments.Remove(r.Context(), a, p["t"], p["ext"], p["id"]); err != nil {
		h.attachmentErr(w, err)
		return
	}
	noContent(w)
}

func (h *Handler) blockAttachments(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	_, all, err := h.o.Attachments.BlockedDocuments(r.Context(), a, p["t"], p["hash"])
	if err != nil {
		h.attachmentErr(w, err)
		return
	}
	heads := map[string]bool{}
	for _, x := range attachment.Heads(all) {
		heads[x.ID] = true
	}
	out := []attachmentJSON{}
	for _, x := range all {
		out = append(out, h.attachmentOf(x, true, heads[x.ID], c))
	}
	reply(w, r, http.StatusOK, map[string]any{"attachments": out}, "", noStore)
}

func (h *Handler) removeBlockAttachment(w http.ResponseWriter, r *http.Request, c caller, p params) {
	a, _ := c.actor()
	if err := h.o.Attachments.RemoveBlockedDocument(r.Context(), a, p["t"], p["hash"], p["id"]); err != nil {
		h.attachmentErr(w, err)
		return
	}
	noContent(w)
}

// --- the index rows' attachments ---

// kindSummary is, per kind, what a caller may read of a row's body: the count and the latest
// summary.
type kindSummary struct {
	Count   int             `json:"count"`
	Latest  json.RawMessage `json:"latest"`
	Expired bool            `json:"expired,omitempty"`
}

// withAttachments fills rows' attachments: the readers-visible heads of each row's body.
func (h *Handler) withAttachments(r *http.Request, c caller, name string, rows ...*releaseOut) error {
	if h.o.Attachments == nil || len(rows) == 0 {
		return nil
	}
	all, err := h.o.Store.ExtensionAttachments(r.Context(), c.tenant.ID, name)
	if err != nil || len(all) == 0 {
		return err
	}
	bodies := map[string]bool{}
	for _, o := range rows {
		bodies[o.BodyHash] = true
	}
	readable := attachment.Readable(all, bodies)
	for _, o := range rows {
		for _, a := range readable {
			if a.BodyHash != o.BodyHash {
				continue
			}
			if o.Attachments == nil {
				o.Attachments = map[string]*kindSummary{}
			}
			k := o.Attachments[a.Kind]
			if k == nil {
				k = &kindSummary{}
				o.Attachments[a.Kind] = k
			}
			k.Count++
			if k.Latest == nil || a.IssuedAt.After(o.latestAt[a.Kind]) {
				k.Latest, k.Expired = json.RawMessage(a.Summary), attachment.Expired(a, h.now())
				if o.latestAt == nil {
					o.latestAt = map[string]time.Time{}
				}
				o.latestAt[a.Kind] = a.IssuedAt
			}
		}
	}
	return nil
}

// attachmentRoute answers 404 on a server without attachments (Options.Attachments nil).
func attachmentRoute(f handler) handler {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, c caller, p params) {
		if h.o.Attachments == nil {
			notFound(w)
			return
		}
		f(h, w, r, c, p)
	}
}
