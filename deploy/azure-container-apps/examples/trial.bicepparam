// A trial: purge protection off and a 7-day soft delete, so that deleting the group and purging
// the vault removes it.
using '../main.bicep'

param prefix = 'kista'
// a version, or until the first release :edge pinned by its digest (ghcr.io/hugr-lab/kista@sha256:…)
param image = 'ghcr.io/hugr-lab/kista:v0.1.0'
// the server administrators' issuer: Microsoft Entra ID, tokens of an app registration whose
// audience is serverAudiences (Entra's v2.0 tokens carry the app's client id)
param serverIssuers = [
  {
    name: 'entra'
    url: 'https://login.microsoftonline.com/00000000-0000-0000-0000-000000000000/v2.0'
    required_claims: { azp: '11111111-1111-1111-1111-111111111111' }
  }
]
param serverAudiences = ['22222222-2222-2222-2222-222222222222']
param serverAdmins = ['subject:entra|33333333-3333-3333-3333-333333333333']
// optional: licence managers, who manage every tenant's licences and nothing else (spec 0029)
// param serverLicenseAdmins = ['subject:entra|44444444-4444-4444-4444-444444444444']
param purgeProtection = false
param softDeleteDays = 7
