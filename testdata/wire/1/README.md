# Wire fixtures, major 1

What kista 0.2 writes on each wire format another kista reads (spec 0046), as its own producers
wrote it once, between a source kista (`https://kista.example`, tenant `acme`, channel `prod`), a
gateway that mirrors it and subscribes to its feed, and a site that plans a bundle:

| File | Written by |
| --- | --- |
| `well-known.json` | the source's `GET <prefix>/.well-known/duckdb-extension-repo.json` |
| `names.json`, `revocations.json` | the source's `GET <prefix>/-/names` and `-/revocations` |
| `attestation-keys.json` | the source's `GET …/tenants/acme/attestation-keys` |
| `feed-request.json` | the gateway's feed client: the body it posts to `…/tenants/acme/feed` |
| `feed-answer.json`, `feed-document.json` | the source's feed route and `…/feed/documents/<id>`: an offer and a withdrawal, signed by the tenant's attestation key |
| `plan.json` | the site's plan, encoded as `kista admin bundle plan` writes it |
| `bundle.kbundle` | the gateway's export for that plan, signed: a file, the source's `.well-known` file, a licence, a revocation, the statements with their document |

Not from a kista producer: the licence inside the bundle is signed as an issuer signs one, with
the repository's test licence key (kista issues no licence; the bundle carries the token as the
gateway stored it). There is no fixture of `-/licenses`. Only public material is here: public keys,
signed statements, a signed manifest; the keys that signed them were made for the occasion.

They are never edited or regenerated: every later release reads them, and must still write every
member they have (`internal/api/wire_test.go`, `TestWireFixtures`). A format's next major gets its
own directory.
