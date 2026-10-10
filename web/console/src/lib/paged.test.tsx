import { describe, expect, it } from 'vitest'
import { act, renderHook, waitFor } from '@testing-library/react'
import { usePaged } from './context'

// Spec 0015 phase 1b: lists by the API's cursors; a change of filter starts at the first page,
// even when it goes back to a filter seen before.
describe('usePaged', () => {
  it('pages by cursor, back and forth, and restarts on another filter', async () => {
    const asked: string[] = []
    const fetchPage = (f: string) => async (cursor: string) => {
      asked.push(`${f}:${cursor}`)
      const n = Number(cursor || '0')
      return { items: [`${f}${n}`], next: n < 3 ? String(n + 1) : undefined }
    }
    const { result, rerender } = renderHook(({ f }) => usePaged(fetchPage(f), [f]), { initialProps: { f: 'h' } })
    await waitFor(() => expect(result.current.items).toEqual(['h0']))
    act(() => result.current.next())
    await waitFor(() => expect(result.current.items).toEqual(['h1']))
    act(() => result.current.next())
    await waitFor(() => expect(result.current.items).toEqual(['h2']))
    expect(result.current.page).toBe(2)
    act(() => result.current.prev())
    await waitFor(() => expect(result.current.items).toEqual(['h1']))
    rerender({ f: 'he' })
    await waitFor(() => expect(result.current.items).toEqual(['he0']))
    rerender({ f: 'h' })
    await waitFor(() => expect(result.current.items).toEqual(['h0']))
    expect(result.current.page).toBe(0)
  })
})
