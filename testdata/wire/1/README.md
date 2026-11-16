# Wire fixtures, major 1

The bytes kista 0.2 writes for each wire format (spec 0046), signed with keys made for the occasion
and thrown away: statements as the feed route answers them with their keys file and document, the
dash routes' answers, a licence, two plans and the signed bundles that answer them.

They are never edited or regenerated: every later release reads them
(`cmd/kista/skew_test.go`, `TestWireFixtures`). A format's next major gets its own directory.
