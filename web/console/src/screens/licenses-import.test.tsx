import { afterEach, beforeAll, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { Client } from '../lib/http'
import { Import, importEntry } from './Licenses'
import type { License, NameKeys } from './Licenses'

// Spec 0035: the import dialog's entry: listed by the licence's key, the only one preselected, never
// one among several, another one typed; what ?source= and If-Match it sends.

const b64 = (s: string) => btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
const token = `${b64('{"alg":"RS256","kid":"sha256:aa"}')}.${b64('{"ext":"tresor","plan":"pro"}')}.sig`
const keys = (...names: string[]): NameKeys[] => [{ name: 'tresor', keys: [{ kid: 'sha256:aa', sources: names.map((n) => {
  const [kind, name] = n.split(':')
  return { kind: kind as 'publisher' | 'upstream', name, builds: 1, distributor: name === 'enterest-ci' ? 'enterest' : undefined }
}) }] }]

describe('importEntry', () => {
  const listed = (...names: string[]) => keys(...names)[0].keys[0].sources
  it('preselects only the one entry listed, never one among several', () => {
    expect(importEntry(listed('publisher:ci'), '', '')).toEqual({ entry: 'publisher:ci', bad: '' })
    expect(importEntry(listed('publisher:ci', 'upstream:org'), '', '')).toEqual({ entry: '', bad: '' })
    expect(importEntry(listed('publisher:ci', 'upstream:org'), 'upstream:org', '')).toEqual({ entry: 'upstream:org', bad: '' })
  })
  it('drops a choice no longer listed', () => {
    expect(importEntry(listed('publisher:ci', 'upstream:org'), 'upstream:gone', '')).toEqual({ entry: '', bad: '' })
    expect(importEntry(listed('publisher:ci'), 'upstream:gone', '')).toEqual({ entry: 'publisher:ci', bad: '' })
  })
  it('takes the typed entry, by its source name', () => {
    expect(importEntry(listed('publisher:ci'), 'other', ' upstream:org ')).toEqual({ entry: 'upstream:org', bad: '' })
    expect(importEntry([], '', 'publisher:x')).toEqual({ entry: 'publisher:x', bad: '' })
    expect(importEntry([], '', 'publisher-id:0190').entry).toBe('')
    expect(importEntry([], '', 'publisher-id:0190').bad).toContain('upstream:<name> or publisher:<name>')
    expect(importEntry([], '', '')).toEqual({ entry: '', bad: '' })
  })
})

describe('Import', () => {
  beforeAll(() => {
    const proto = HTMLDialogElement.prototype as { showModal?: () => void; close: () => void }
    proto.showModal ??= function (this: HTMLDialogElement) {
      this.setAttribute('open', '')
    }
    proto.close = function (this: HTMLDialogElement) {
      this.removeAttribute('open')
    }
  })
  afterEach(() => {
    cleanup()
    vi.unstubAllGlobals()
  })

  function api() {
    const calls: { method: string; path: string; ifMatch: string | null }[] = []
    vi.stubGlobal('fetch', vi.fn(async (input: string, init?: RequestInit) => {
      const u = new URL(input)
      calls.push({ method: init?.method ?? 'GET', path: u.pathname + u.search, ifMatch: new Headers(init?.headers).get('If-Match') })
      return new Response(JSON.stringify({ name: 'tresor', entry: 'publisher:ci', etag: '"v2"' }), { status: 201 })
    }))
    return calls
  }
  const client = () => new Client({ base: 'https://k.example/api/v1', prefix: '/tenants/', getToken: async () => 'tok' })
  const pickFile = async () => {
    // jsdom's File has no text()
    const f = Object.assign(new File([token], 'tresor.lic'), { text: async () => token })
    fireEvent.change(screen.getByLabelText('licence file'), { target: { files: [f] } })
    await screen.findByText(/plan pro/)
  }
  const importButton = () => screen.getByRole('button', { name: /^(Import|Replace…)$/ }) as HTMLButtonElement

  it('lists two entries, preselects neither, and sends the one chosen', async () => {
    const calls = api()
    const onImported = vi.fn()
    render(<Import client={client()} api="/tenants/acme" licenses={[]} keys={keys('publisher:enterest-ci', 'upstream:org')} keysLoading={false}
      onImported={onImported} onStale={vi.fn()} />)
    await pickFile()
    const radios = screen.getAllByRole('radio') as HTMLInputElement[]
    expect(radios.map((r) => [r.value, r.checked])).toEqual([['publisher:enterest-ci', false], ['upstream:org', false], ['other', false]])
    expect(screen.getByRole('radiogroup', { name: /several declare its key/ })).toBeTruthy()
    expect(screen.getByText("enterest's publisher (a distributor)")).toBeTruthy()
    expect(importButton().disabled).toBe(true)
    fireEvent.click(screen.getByRole('radio', { name: /upstream:org/ }))
    fireEvent.click(importButton())
    await waitFor(() => expect(onImported).toHaveBeenCalledTimes(1))
    expect(calls).toEqual([{ method: 'POST', path: '/api/v1/tenants/acme/licenses?source=upstream%3Aorg', ifMatch: null }])
  })

  it('preselects the only entry listed, and replaces its licence with If-Match after a confirmation', async () => {
    const calls = api()
    const stored = { name: 'tresor', entry: 'publisher:ci', etag: '"v7"', source: 'import' } as License
    render(<Import client={client()} api="/tenants/acme" licenses={[stored]} keys={keys('publisher:ci')} keysLoading={false}
      onImported={vi.fn()} onStale={vi.fn()} />)
    await pickFile()
    expect((screen.getByRole('radio', { name: /publisher:ci/ }) as HTMLInputElement).checked).toBe(true)
    expect(screen.getByText(/It replaces the licence stored for tresor in publisher:ci/)).toBeTruthy()
    fireEvent.click(importButton())
    fireEvent.click(screen.getByRole('button', { name: 'Replace' }))
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0]).toEqual({ method: 'POST', path: '/api/v1/tenants/acme/licenses?source=publisher%3Aci', ifMatch: '"v7"' })
  })

  it('with no entry listed, sends the one typed; an id form is refused before any call', async () => {
    const calls = api()
    render(<Import client={client()} api="/tenants/acme" licenses={[]} keys={keys()} keysLoading={false} onImported={vi.fn()} onStale={vi.fn()} />)
    await pickFile()
    expect(screen.getByText(/No entry's live builds declare this licence's key/)).toBeTruthy()
    const input = screen.getByLabelText('Into the entry')
    fireEvent.change(input, { target: { value: 'publisher-id:0190' } })
    expect(screen.getByText(/Name the entry by its source/)).toBeTruthy()
    expect(importButton().disabled).toBe(true)
    fireEvent.change(input, { target: { value: 'publisher:enterest-ci' } })
    fireEvent.click(importButton())
    await waitFor(() => expect(calls).toHaveLength(1))
    expect(calls[0].path).toBe('/api/v1/tenants/acme/licenses?source=publisher%3Aenterest-ci')
  })

  it('waits for the keys before saying none is listed', async () => {
    api()
    render(<Import client={client()} api="/tenants/acme" licenses={[]} keysLoading onImported={vi.fn()} onStale={vi.fn()} />)
    await pickFile()
    expect(screen.queryByText(/No entry's live builds declare/)).toBeNull()
    expect(importButton().disabled).toBe(true)
  })
})
