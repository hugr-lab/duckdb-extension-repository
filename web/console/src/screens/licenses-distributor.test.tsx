import { afterEach, beforeAll, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Perms, TenantContext } from '../lib/context'
import { Client } from '../lib/http'
import { Import, importEntry, LicenseView, readOnly } from './Licenses'
import type { License, NameKeys } from './Licenses'

// Spec 0037: a distributor's publisher's entry is its licence manager's and the server
// administrators' to write: to a tenant administrator its licence is read-only, and the import
// dialog lists the entry without letting them choose it.

const b64 = (s: string) => btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
const token = `${b64('{"alg":"RS256","kid":"sha256:aa"}')}.${b64('{"ext":"tresor","plan":"pro"}')}.sig`
const keys = (...names: string[]): NameKeys[] => [{ name: 'tresor', keys: [{ kid: 'sha256:aa', sources: names.map((n) => {
  const [kind, name] = n.split(':')
  return { kind: kind as 'publisher' | 'upstream', name, builds: 1, distributor: name === 'enterest-ci' ? 'enterest' : undefined }
}) }] }]
const client = () => new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
const licence = (distributor?: string) => ({ name: 'tresor', entry: distributor ? 'publisher:enterest-ci' : 'publisher:ci', id: 'lic-1',
  issuer: 'https://issuer.example', holder_id: 'acme', plan: 'pro', issued_at: '2026-10-01T00:00:00Z', expires_at: '2026-12-01T00:00:00Z',
  grace_until: '2026-12-08T00:00:00Z', key: 'sha256:aa', source: 'import', visibility: 'private', status: 'valid', etag: '"v1"', distributor }) as License

beforeAll(() => {
  const proto = HTMLDialogElement.prototype as { showModal?: () => void; close: () => void }
  proto.showModal ??= function (this: HTMLDialogElement) {
    this.setAttribute('open', '')
  }
  proto.close = function (this: HTMLDialogElement) {
    this.removeAttribute('open')
  }
})
afterEach(cleanup)

describe('readOnly', () => {
  it("is a distributor's licence or entry, to a caller who is not a server administrator", () => {
    expect(readOnly({ distributor: 'enterest' }, false)).toBe(true)
    expect(readOnly({ distributor: 'enterest' }, true)).toBe(false)
    expect(readOnly({}, false)).toBe(false)
  })
  it('never chooses nor preselects it for a tenant administrator', () => {
    const listed = (...names: string[]) => keys(...names)[0].keys[0].sources
    expect(importEntry(listed('publisher:enterest-ci'), '', '', false)).toEqual({ entry: '', bad: '' })
    expect(importEntry(listed('publisher:enterest-ci'), '', '', true)).toEqual({ entry: 'publisher:enterest-ci', bad: '' })
    expect(importEntry(listed('publisher:enterest-ci', 'upstream:org'), 'publisher:enterest-ci', '', false)).toEqual({ entry: '', bad: '' })
    expect(importEntry(listed('publisher:enterest-ci', 'upstream:org'), '', '', false)).toEqual({ entry: '', bad: '' }) // never among several
    expect(importEntry(listed('publisher:enterest-ci', 'upstream:org'), 'upstream:org', '', false)).toEqual({ entry: 'upstream:org', bad: '' })
    expect(importEntry(listed('publisher:enterest-ci'), 'other', 'upstream:x', false)).toEqual({ entry: 'upstream:x', bad: '' })
  })
})

describe('LicenseView', () => {
  const view = (l: License, server: boolean) => render(
    <MemoryRouter>
      <TenantContext.Provider value={{ client: client(), tenant: 'acme', perms: new Perms(server), api: '/tenants/acme', base: '/t/acme' }}>
        <LicenseView l={l} server={server} onChanged={() => {}} onRemoved={() => {}} />
      </TenantContext.Provider>
    </MemoryRouter>)
  it("shows a distributor's licence read-only to a tenant administrator", () => {
    view(licence('enterest'), false)
    expect(screen.queryByRole('button', { name: /Remove/ })).toBeNull()
    expect(screen.queryByRole('button', { name: /Make public/ })).toBeNull()
    expect(screen.getByText(/enterest's licence manager writes it, or a server administrator/)).toBeTruthy()
  })
  it('lets a server administrator write it, and a tenant administrator their own', () => {
    view(licence('enterest'), true)
    expect(screen.getByRole('button', { name: /Remove/ })).toBeTruthy()
    expect(screen.getByRole('button', { name: /Make public/ })).toBeTruthy()
    cleanup()
    view(licence(), false)
    expect(screen.getByRole('button', { name: /Remove/ })).toBeTruthy()
    expect(screen.queryByText(/licence manager writes it/)).toBeNull()
  })
})

describe('Import', () => {
  const pickFile = async () => {
    const f = Object.assign(new File([token], 'tresor.lic'), { text: async () => token })
    fireEvent.change(screen.getByLabelText('licence file'), { target: { files: [f] } })
    await screen.findByText(/plan pro/)
  }
  const importButton = () => screen.getByRole('button', { name: /^(Import|Replace…)$/ }) as HTMLButtonElement
  const radio = (name: RegExp) => screen.getByRole('radio', { name }) as HTMLInputElement

  it("lists a distributor's entry without letting a tenant administrator choose it", async () => {
    render(<Import client={client()} api="/tenants/acme" licenses={[]} keys={keys('publisher:enterest-ci', 'upstream:org')} keysLoading={false}
      onImported={() => {}} onStale={() => {}} />)
    await pickFile()
    expect(radio(/publisher:enterest-ci/).disabled).toBe(true)
    expect(radio(/upstream:org/).disabled).toBe(false)
    expect(screen.getByText(/a distributor's: its licence manager writes it/)).toBeTruthy()
  })
  it('never preselects it, alone, for a tenant administrator; a server administrator chooses it', async () => {
    render(<Import client={client()} api="/tenants/acme" licenses={[]} keys={keys('publisher:enterest-ci')} keysLoading={false}
      onImported={() => {}} onStale={() => {}} />)
    await pickFile()
    expect(radio(/publisher:enterest-ci/).checked).toBe(false)
    expect(importButton().disabled).toBe(true)
    cleanup()
    render(<Import client={client()} api="/tenants/acme" server licenses={[]} keys={keys('publisher:enterest-ci')} keysLoading={false}
      onImported={() => {}} onStale={() => {}} />)
    await pickFile()
    expect(radio(/publisher:enterest-ci/).checked).toBe(true)
    expect(radio(/publisher:enterest-ci/).disabled).toBe(false)
    expect(importButton().disabled).toBe(false)
  })
})
