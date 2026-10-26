import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { useState } from 'react'
import { Warnings } from '../components/ui'
import { Client, type Warning, warningsOf } from './http'

// Spec 0035 phase 2: an act's answer carries its licence warnings in one generic array; the client
// hands a change's to onWarnings, and the console shows the last act's above the content until
// dismissed. Advisory: the act went through.

const warning: Warning = {
  name: 'tresor',
  source: 'publisher:enterest-ci',
  channel: 'prod',
  message: 'publisher:enterest-ci holds a licence for tresor and has a public release in channel prod: (advisory: nothing was refused)',
}

function answer(body: unknown, status = 200) {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), { status })))
}

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})

describe('warningsOf', () => {
  it('reads the warnings member, each as the API writes it', () => {
    expect(warningsOf({ name: 'tresor', warnings: [warning] })).toEqual([warning])
    expect(warningsOf({ name: 'tresor' })).toEqual([])
    expect(warningsOf(null)).toEqual([])
    expect(warningsOf({ warnings: 'no' })).toEqual([])
    expect(warningsOf({ warnings: [{ name: 'tresor' }, warning, null] })).toEqual([warning])
  })
})

describe('the client', () => {
  it("hands each change's warnings to onWarnings, none included; a read's are not handed", async () => {
    const onWarnings = vi.fn()
    const c = new Client({ base: '/api/v1', prefix: '/tenants/acme/', getToken: async () => 't', onWarnings })
    answer({ name: 'tresor', visibility: 'public', warnings: [warning] })
    const r = await c.post<{ visibility: string }>('/tenants/acme/licenses/tresor/public?source=publisher:enterest-ci')
    expect(r.data.visibility).toBe('public')
    expect(onWarnings).toHaveBeenCalledTimes(1)
    expect(onWarnings).toHaveBeenCalledWith([warning])
    answer({ name: 'tresor' })
    await c.post('/tenants/acme/licenses/tresor/private?source=publisher:enterest-ci')
    expect(onWarnings).toHaveBeenLastCalledWith([]) // the last change's: none
    answer({ name: 'tresor', warnings: [warning] })
    await c.get('/tenants/acme/licenses/tresor')
    expect(onWarnings).toHaveBeenCalledTimes(2)
    // a failed change clears them before its error is thrown
    answer({ detail: 'no' }, 409)
    await expect(c.post('/tenants/acme/licenses/tresor/public?source=publisher:enterest-ci')).rejects.toMatchObject({ status: 409 })
    expect(onWarnings).toHaveBeenCalledTimes(3)
    expect(onWarnings).toHaveBeenLastCalledWith([])
    // and so does one that never reached kista
    onWarnings.mockClear()
    vi.stubGlobal('fetch', vi.fn(async () => { throw new TypeError('Failed to fetch') }))
    await expect(c.post('/tenants/acme/licenses/tresor/public?source=publisher:enterest-ci')).rejects.toThrow('Failed to fetch')
    expect(onWarnings).toHaveBeenCalledExactlyOnceWith([])
    await expect(c.get('/tenants/acme/licenses/tresor')).rejects.toThrow('Failed to fetch')
    expect(onWarnings).toHaveBeenCalledTimes(1)
  })
})

describe('Warnings', () => {
  function Shown({ initial }: { initial: Warning[] }) {
    const [ws, setWs] = useState(initial)
    return (
      <main>
        <h1>Licences</h1>
        <Warnings warnings={ws} onDismiss={() => setWs([])} />
      </main>
    )
  }

  it('shows each warning, says nothing was refused, and is dismissed, the focus on the heading', () => {
    render(<Shown initial={[warning, { ...warning, channel: 'nightly', message: 'in nightly too' }]} />)
    const box = screen.getByRole('status', { name: 'Warnings' })
    expect(box.textContent).toContain('Nothing was refused')
    expect(box.textContent).toContain(warning.message)
    expect(box.textContent).toContain('in nightly too')
    expect(screen.queryByRole('alert')).toBeNull() // advisory, not a failure
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }))
    expect(screen.getByRole('status', { name: 'Warnings' }).textContent).toBe('') // the region stays, empty
    expect(document.activeElement).toBe(screen.getByRole('heading', { name: 'Licences' }))
  })

  it('keeps an empty status region without warnings', () => {
    render(<Warnings warnings={[]} onDismiss={() => {}} />)
    expect(screen.getByRole('status', { name: 'Warnings' }).textContent).toBe('')
    expect(screen.queryByRole('button', { name: 'Dismiss' })).toBeNull()
  })
})
