// Package attachment holds documents about a body in a tenant (spec 0011): their kinds and media
// types, the summaries computed at intake, the open review schema, and the service that attaches,
// lists, reads and removes them.
package attachment

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hugr-lab/duckdb-extension-repository/internal/store"
)

// Media types (canonical forms, as stored and served).
const (
	CycloneDX   = "application/vnd.cyclonedx+json"
	SPDX        = "application/spdx+json"
	OpenVEX     = "application/openvex+json"
	SARIF       = "application/sarif+json"
	OSVScanner  = "application/json; profile=osv-scanner"
	Review      = "application/vnd.kista.review.v1+json"
	Sigstore    = "application/vnd.dev.sigstore.bundle.v0.3+json"
	DSSE        = "application/vnd.dsse.envelope.v1+json"
	Markdown    = "text/markdown"
	maxDepth    = 32
	maxSummary  = 4000
	maxTitle    = 200
	maxListSize = 100000 // components, findings, packages counted at most
)

// mediaTypes are the media types each kind takes (spec 0011).
var mediaTypes = map[string][]string{
	store.KindSBOM:            {CycloneDX, SPDX},
	store.KindVulnerabilities: {CycloneDX, OpenVEX, SARIF, OSVScanner},
	store.KindReview:          {Review},
	store.KindProvenance:      {Sigstore, DSSE},
	store.KindNotes:           {Markdown},
}

// Kinds lists the kinds.
var Kinds = []string{store.KindSBOM, store.KindVulnerabilities, store.KindReview, store.KindProvenance, store.KindNotes}

// ErrDocument is a document that is not what its kind and media type say.
var ErrDocument = errors.New("attachment: the document")

// CanonicalMediaType answers a kind's canonical form of a declared media type (parameters such as
// charset dropped, except osv-scanner's profile), or an error.
func CanonicalMediaType(kind, declared string) (string, error) {
	mt, params, err := mime.ParseMediaType(declared)
	if err != nil {
		return "", fmt.Errorf("%w: media type %q", store.ErrInvalid, declared)
	}
	if mt == "application/json" && params["profile"] == "osv-scanner" {
		mt = OSVScanner
	}
	for _, m := range mediaTypes[kind] {
		if m == mt {
			return m, nil
		}
	}
	if _, ok := mediaTypes[kind]; !ok {
		return "", fmt.Errorf("%w: kind %q (%s)", store.ErrInvalid, kind, strings.Join(Kinds, ", "))
	}
	return "", fmt.Errorf("%w: a %s is %s, not %q", store.ErrInvalid, kind, strings.Join(mediaTypes[kind], " or "), declared)
}

// ValidTitle checks a title: at most 200 bytes of printable text.
func ValidTitle(t string) error {
	if t == "" || len(t) > maxTitle || !utf8.ValidString(t) || strings.ContainsFunc(t, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		return fmt.Errorf("%w: a title is 1 to %d bytes of printable text", store.ErrInvalid, maxTitle)
	}
	return nil
}

// Summarize checks a document against its kind and media type and computes its summary (spec 0011:
// once, at intake). A document it cannot summarise is refused.
func Summarize(kind, mediaType string, content []byte) (string, error) {
	var sum map[string]any
	var err error
	if kind == store.KindNotes {
		sum, err = notes(content)
	} else {
		if err := checkJSON(content); err != nil {
			return "", err
		}
		switch mediaType {
		case CycloneDX:
			if kind == store.KindSBOM {
				sum, err = cyclonedxSBOM(content)
			} else {
				sum, err = cyclonedxVulns(content)
			}
		case SPDX:
			sum, err = spdx(content)
		case OpenVEX:
			sum, err = openvex(content)
		case SARIF:
			sum, err = sarif(content)
		case OSVScanner:
			sum, err = osvScanner(content)
		case Review:
			sum, err = review(content)
		case Sigstore, DSSE:
			sum, err = provenance(mediaType, content)
		default:
			err = fmt.Errorf("%w: media type %s", ErrDocument, mediaType)
		}
	}
	if err != nil {
		return "", err
	}
	// a document's strings reach terminals and pages: no control or format character (bidi
	// overrides, C1 escapes), and no HTML escaping that would grow them
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(scrub(sum)); err != nil {
		return "", err
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	if len(b) > maxSummary {
		return "", fmt.Errorf("%w: its summary is over %d bytes", ErrDocument, maxSummary)
	}
	return string(b), nil
}

// checkJSON refuses what is not one JSON value within maxDepth levels.
func checkJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	depth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: not JSON: %v", ErrDocument, err)
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
				if depth > maxDepth {
					return fmt.Errorf("%w: JSON nested over %d levels", ErrDocument, maxDepth)
				}
			} else {
				depth--
			}
		}
	}
	if depth != 0 {
		return fmt.Errorf("%w: not JSON", ErrDocument)
	}
	return nil
}

func notes(b []byte) (map[string]any, error) {
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("%w: Markdown is UTF-8", ErrDocument)
	}
	first := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	first = strings.TrimLeft(first, "# ")
	if len(first) > 200 {
		first = strings.ToValidUTF8(first[:200], "")
	}
	return map[string]any{"first_line": first}, nil
}

// --- SBOMs ---

type cdxComponent struct {
	Type       string         `json:"type"`
	Licenses   []cdxLicense   `json:"licenses"`
	Components []cdxComponent `json:"components"`
}

type cdxLicense struct {
	License struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"license"`
	Expression string `json:"expression"`
}

func cyclonedxSBOM(b []byte) (map[string]any, error) {
	var d struct {
		BOMFormat   string         `json:"bomFormat"`
		SpecVersion string         `json:"specVersion"`
		Components  []cdxComponent `json:"components"`
	}
	if err := json.Unmarshal(b, &d); err != nil || d.BOMFormat != "CycloneDX" {
		return nil, fmt.Errorf("%w: not a CycloneDX BOM", ErrDocument)
	}
	if !strings.HasPrefix(d.SpecVersion, "1.") {
		return nil, fmt.Errorf("%w: CycloneDX %q", ErrDocument, d.SpecVersion)
	}
	licences := map[string]bool{}
	components, crypto := 0, 0
	var walk func([]cdxComponent)
	walk = func(cs []cdxComponent) {
		for _, c := range cs {
			if components >= maxListSize {
				return
			}
			components++
			if c.Type == "cryptographic-asset" {
				crypto++
			}
			for _, l := range c.Licenses {
				if id := firstOf(l.License.ID, l.License.Name, l.Expression); id != "" && len(licences) < 1000 {
					licences[id] = true
				}
			}
			walk(c.Components)
		}
	}
	walk(d.Components)
	sum := map[string]any{"format": "cyclonedx " + d.SpecVersion, "components": components, "licences": len(licences)}
	if crypto > 0 {
		sum["crypto_assets"] = crypto
	}
	return sum, nil
}

func spdx(b []byte) (map[string]any, error) {
	var d struct {
		SPDXVersion string `json:"spdxVersion"`
		Packages    []struct {
			LicenseConcluded string `json:"licenseConcluded"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &d); err != nil || !strings.HasPrefix(d.SPDXVersion, "SPDX-2.") {
		return nil, fmt.Errorf("%w: not an SPDX 2 document", ErrDocument)
	}
	licences := map[string]bool{}
	for _, p := range d.Packages {
		if p.LicenseConcluded != "" && p.LicenseConcluded != "NOASSERTION" && len(licences) < 1000 {
			licences[p.LicenseConcluded] = true
		}
	}
	return map[string]any{"format": strings.ToLower(d.SPDXVersion), "components": min(len(d.Packages), maxListSize), "licences": len(licences)}, nil
}

// --- vulnerability reports: findings by severity ---

var severities = []string{"critical", "high", "medium", "low", "unknown"}

func severity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "critical", "high", "medium", "low":
		return s
	case "moderate":
		return "medium"
	case "error":
		return "high"
	case "warning":
		return "medium"
	case "note", "info", "none":
		return "low"
	}
	return "unknown"
}

func vulnSummary(counts map[string]int, notAffected int, scannedAt string) map[string]any {
	sum := map[string]any{}
	total := 0
	for _, s := range severities {
		if counts[s] > 0 {
			sum[s] = counts[s]
			total += counts[s]
		}
	}
	sum["findings"] = total
	if notAffected > 0 {
		sum["not_affected"] = notAffected
	}
	if t, err := time.Parse(time.RFC3339, scannedAt); err == nil {
		sum["scanned_at"] = t.UTC().Format(time.RFC3339)
	}
	return sum
}

func cyclonedxVulns(b []byte) (map[string]any, error) {
	var d struct {
		BOMFormat string `json:"bomFormat"`
		Metadata  struct {
			Timestamp string `json:"timestamp"`
		} `json:"metadata"`
		Vulnerabilities []struct {
			Ratings []struct {
				Severity string `json:"severity"`
			} `json:"ratings"`
			Analysis struct {
				State string `json:"state"`
			} `json:"analysis"`
		} `json:"vulnerabilities"`
	}
	if err := json.Unmarshal(b, &d); err != nil || d.BOMFormat != "CycloneDX" {
		return nil, fmt.Errorf("%w: not a CycloneDX VDR or VEX", ErrDocument)
	}
	counts, notAffected := map[string]int{}, 0
	for i, v := range d.Vulnerabilities {
		if i >= maxListSize {
			break
		}
		if v.Analysis.State == "not_affected" || v.Analysis.State == "false_positive" {
			notAffected++
			continue
		}
		worst := "unknown"
		for _, r := range v.Ratings {
			if s := severity(r.Severity); rank(s) < rank(worst) {
				worst = s
			}
		}
		counts[worst]++
	}
	return vulnSummary(counts, notAffected, d.Metadata.Timestamp), nil
}

func rank(s string) int {
	for i, x := range severities {
		if x == s {
			return i
		}
	}
	return len(severities)
}

func openvex(b []byte) (map[string]any, error) {
	var d struct {
		Context    string `json:"@context"`
		Timestamp  string `json:"timestamp"`
		Statements []struct {
			Status string `json:"status"`
		} `json:"statements"`
	}
	if err := json.Unmarshal(b, &d); err != nil || !strings.Contains(d.Context, "openvex") {
		return nil, fmt.Errorf("%w: not an OpenVEX document", ErrDocument)
	}
	counts, notAffected := map[string]int{}, 0
	for i, s := range d.Statements {
		if i >= maxListSize {
			break
		}
		switch s.Status {
		case "not_affected", "fixed":
			notAffected++
		default: // affected, under_investigation: OpenVEX carries no severity
			counts["unknown"]++
		}
	}
	return vulnSummary(counts, notAffected, d.Timestamp), nil
}

func sarif(b []byte) (map[string]any, error) {
	var d struct {
		Version string `json:"version"`
		Runs    []struct {
			Invocations []struct {
				EndTimeUTC string `json:"endTimeUtc"`
			} `json:"invocations"`
			Results []struct {
				Level string `json:"level"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(b, &d); err != nil || d.Version != "2.1.0" {
		return nil, fmt.Errorf("%w: not SARIF 2.1.0", ErrDocument)
	}
	counts, n, scanned := map[string]int{}, 0, ""
	for _, r := range d.Runs {
		for _, x := range r.Results {
			if n >= maxListSize {
				break
			}
			n++
			lvl := x.Level
			if lvl == "" {
				lvl = "warning" // SARIF's default level
			}
			counts[severity(lvl)]++
		}
		for _, inv := range r.Invocations {
			if inv.EndTimeUTC > scanned {
				scanned = inv.EndTimeUTC
			}
		}
	}
	return vulnSummary(counts, 0, scanned), nil
}

func osvScanner(b []byte) (map[string]any, error) {
	var d struct {
		Results *[]struct {
			Packages []struct {
				Groups []struct {
					MaxSeverity string `json:"max_severity"`
				} `json:"groups"`
				Vulnerabilities []json.RawMessage `json:"vulnerabilities"`
			} `json:"packages"`
		} `json:"results"`
	}
	if err := json.Unmarshal(b, &d); err != nil || d.Results == nil {
		return nil, fmt.Errorf("%w: not osv-scanner output", ErrDocument)
	}
	counts := map[string]int{}
	for _, r := range *d.Results {
		for _, p := range r.Packages {
			if len(p.Groups) == 0 {
				counts["unknown"] += min(len(p.Vulnerabilities), maxListSize)
			}
			for _, g := range p.Groups {
				counts[cvssSeverity(g.MaxSeverity)]++
			}
		}
	}
	return vulnSummary(counts, 0, ""), nil
}

// cvssSeverity buckets a CVSS score as CVSS v3 does.
func cvssSeverity(score string) string {
	var f float64
	if _, err := fmt.Sscanf(score, "%g", &f); err != nil {
		return "unknown"
	}
	switch {
	case f >= 9:
		return "critical"
	case f >= 7:
		return "high"
	case f >= 4:
		return "medium"
	case f > 0:
		return "low"
	}
	return "unknown"
}

// --- the review schema: kista.review.v1 ---

// ReviewV1 is the open review format (spec 0011): any analyser produces it, kista renders only it.
type ReviewV1 struct {
	Schema       string          `json:"schema"`
	Verdict      string          `json:"verdict"`
	AnalysedAt   string          `json:"analysed_at"`
	Analyser     string          `json:"analyser"`
	Capabilities []string        `json:"capabilities"`
	Network      []string        `json:"network"`
	Files        []string        `json:"files"`
	Processes    []string        `json:"processes"`
	Licences     []string        `json:"licences"`
	Findings     []ReviewFinding `json:"findings"`
	Text         string          `json:"text"`
}

// ReviewFinding is one finding of a review.
type ReviewFinding struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

func review(b []byte) (map[string]any, error) {
	var r ReviewV1
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || r.Schema != "kista.review.v1" {
		return nil, fmt.Errorf("%w: not a kista.review.v1 document", ErrDocument)
	}
	switch r.Verdict {
	case "approved", "conditional", "rejected":
	default:
		return nil, fmt.Errorf("%w: a review's verdict is approved, conditional or rejected", ErrDocument)
	}
	at, err := time.Parse(time.RFC3339, r.AnalysedAt)
	if err != nil || r.Analyser == "" || len(r.Analyser) > 200 || len(r.Text) > 1<<20 {
		return nil, fmt.Errorf("%w: a review names its analyser and analysed_at (RFC 3339), and its text is at most 1 MiB", ErrDocument)
	}
	for _, list := range [][]string{r.Capabilities, r.Network, r.Files, r.Processes, r.Licences} {
		if len(list) > 1000 {
			return nil, fmt.Errorf("%w: a review list holds at most 1000 entries", ErrDocument)
		}
		for _, s := range list {
			if len(s) > 400 {
				return nil, fmt.Errorf("%w: a review entry is at most 400 bytes", ErrDocument)
			}
		}
	}
	counts := map[string]int{}
	for _, f := range r.Findings {
		if len(f.Title) > 400 || len(f.Detail) > 8000 {
			return nil, fmt.Errorf("%w: a review finding's title is at most 400 bytes, its detail 8000", ErrDocument)
		}
		counts[severity(f.Severity)]++
	}
	analyser := r.Analyser
	sum := map[string]any{"verdict": r.Verdict, "analysed_at": at.UTC().Format(time.RFC3339), "analyser": analyser}
	if len(r.Findings) > 0 {
		for _, s := range severities {
			if counts[s] > 0 {
				sum[s] = counts[s]
			}
		}
		sum["findings"] = len(r.Findings)
	}
	return sum, nil
}

// --- provenance: a Sigstore bundle or a DSSE envelope over an in-toto statement ---

func provenance(mediaType string, b []byte) (map[string]any, error) {
	var env struct {
		PayloadType string `json:"payloadType"`
		Payload     string `json:"payload"`
	}
	if mediaType == Sigstore {
		var bundle struct {
			MediaType    string          `json:"mediaType"`
			DSSEEnvelope json.RawMessage `json:"dsseEnvelope"`
		}
		if err := json.Unmarshal(b, &bundle); err != nil || !strings.HasPrefix(bundle.MediaType, "application/vnd.dev.sigstore.bundle") || bundle.DSSEEnvelope == nil {
			return nil, fmt.Errorf("%w: not a Sigstore bundle with a DSSE envelope", ErrDocument)
		}
		b = bundle.DSSEEnvelope
	}
	if err := json.Unmarshal(b, &env); err != nil || env.PayloadType != "application/vnd.in-toto+json" {
		return nil, fmt.Errorf("%w: not a DSSE envelope of an in-toto statement", ErrDocument)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("%w: the envelope's payload is not base64", ErrDocument)
	}
	if err := checkJSON(payload); err != nil {
		return nil, err
	}
	var st struct {
		Type          string `json:"_type"`
		PredicateType string `json:"predicateType"`
		Predicate     struct {
			RunDetails struct {
				Builder struct {
					ID string `json:"id"`
				} `json:"builder"`
			} `json:"runDetails"`
			BuildDefinition struct {
				ExternalParameters struct {
					Workflow struct {
						Repository string `json:"repository"`
						Ref        string `json:"ref"`
					} `json:"workflow"`
				} `json:"externalParameters"`
			} `json:"buildDefinition"`
		} `json:"predicate"`
	}
	if err := json.Unmarshal(payload, &st); err != nil || !strings.HasPrefix(st.Type, "https://in-toto.io/Statement/") {
		return nil, fmt.Errorf("%w: the payload is not an in-toto statement", ErrDocument)
	}
	sum := map[string]any{"predicate_type": clip(st.PredicateType), "verified": false}
	if v := st.Predicate.RunDetails.Builder.ID; v != "" {
		sum["builder"] = clip(v)
	}
	if v := st.Predicate.BuildDefinition.ExternalParameters.Workflow.Repository; v != "" {
		sum["repository"] = clip(v)
	}
	if v := st.Predicate.BuildDefinition.ExternalParameters.Workflow.Ref; v != "" {
		sum["ref"] = clip(v)
	}
	return sum, nil
}

// scrub drops the control and format characters of a summary's strings.
func scrub(v any) any {
	switch x := v.(type) {
	case string:
		return strings.Map(func(r rune) rune {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, x)
	case map[string]any:
		for k, e := range x {
			x[k] = scrub(e)
		}
	case []any:
		for i, e := range x {
			x[i] = scrub(e)
		}
	}
	return v
}

func clip(s string) string {
	if len(s) > 400 {
		return strings.ToValidUTF8(s[:400], "")
	}
	return s
}

func firstOf(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return clip(s)
		}
	}
	return ""
}
