# Wire fixtures, major 1

What kista 0.2 writes on each wire format another kista reads (spec 0046), as its own producers
wrote it once, between a source kista (`https://kista.example`, tenant `acme`, channel `prod`), a
gateway that mirrors it and subscribes to its feed, and a site that had imported one bundle already
and plans its next:

| File | Written by |
| --- | --- |
| `well-known.json` | the source's `GET <prefix>/.well-known/duckdb-extension-repo.json` |
| `names.json`, `revocations.json` | the source's `GET <prefix>/-/names` and `-/revocations` |
| `attestation-keys.json` | the source's `GET …/tenants/acme/attestation-keys` |
| `feed-request.json` | the gateway's feed client: the body it posts to `…/tenants/acme/feed`, with a body it has statements of (`after_seq` 1) and a new one |
| `feed-answer.json`, `feed-document.json` | the source's feed route and `…/feed/documents/<id>`: an offer and a withdrawal, signed by the tenant's attestation key |
| `plan.json` | the site's plan, encoded as `kista admin bundle plan` writes it: a cell it holds (its etag and body hash, its entry's versions) and a feed body with the last statement received |
| `bundle.kbundle` | the gateway's export for that plan, signed: a file, the source's `.well-known` file, a licence, a revocation, the statements with their document |
| `SHA256SUMS` | the SHA-256 of each file above, as written |

Not from a kista producer: the licence inside the bundle is signed as an issuer signs one, with
the repository's test licence key (kista issues no licence; the bundle carries the token as the
gateway stored it). There is no fixture of `-/licenses`. Only public material is here: public keys,
signed statements, a signed manifest; the keys that signed them were made for the occasion and
thrown away.

They are never edited or regenerated. `internal/api/wire_test.go` (`TestWireFixtures`), at every
run:

- checks each file against `SHA256SUMS`, so a hand edit fails;
- makes the same things again with the release's producers and fails if one no longer writes a
  member a fixture has;
- reads them with the release's readers: a mirror's run over `well-known.json`, `names.json` and
  `revocations.json`; a feed's run over the keys file, the feed's answer and the document; an
  import of `bundle.kbundle` with its licence, its revocation and its statements;
- posts `feed-request.json`'s bytes to a source's feed route, which answers them;
- gives `plan.json` to an exporter, which takes its request. The plan pins the keys of the source
  the fixtures were made with, so a later run's source does not match them and the export carries
  nothing of it: only the plan's reading is checked there, not a bundle made from it.

A format's next major gets its own directory.
