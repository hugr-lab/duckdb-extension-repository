import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { base64Of, codePoints, EntrySummary, iconBlob, MetaDialog, mayEditMeta, metaBody } from './Catalogue'

// Spec 0036: an entry's icon and summary in the console.

afterEach(cleanup)

describe('entry metadata', () => {
  it('shows an icon only as the allow-listed type, whatever the bytes came with', () => {
    expect(iconBlob(new Blob(['<svg onload=alert(1)>'], { type: 'image/svg+xml' })).type).toBe('image/png')
    expect(iconBlob(new Blob(['<html>'], { type: 'text/html' })).type).toBe('image/png')
    expect(iconBlob(new Blob(['x'])).type).toBe('image/png')
  })
  it('sends the whole metadata: a summary-only edit keeps the icon by its hash', () => {
    expect(metaBody(' Geometry ', { keep: 'ab'.repeat(32) })).toEqual({ summary: 'Geometry', icon: { sha256: 'ab'.repeat(32) } })
    expect(metaBody('', null)).toEqual({ summary: null, icon: null })
    expect(metaBody('x', { file: new Uint8Array([137, 80, 78, 71]) })).toEqual({ summary: 'x', icon: { content_type: 'image/png', data: 'iVBORw==' } })
  })
  it('encodes large files in base64', () => {
    const big = new Uint8Array(200_000).fill(65)
    expect(atob(base64Of(big)).length).toBe(200_000)
  })
  it("never offers to edit a distributor's entry, nor to a caller who is no tenant administrator", () => {
    expect(mayEditMeta({ source: 'publisher:ci' }, true)).toBe(true)
    expect(mayEditMeta({ source: 'publisher:enterest-ci', distributor: 'enterest' }, true)).toBe(false)
    expect(mayEditMeta({ source: 'publisher:ci' }, false)).toBe(false)
    expect(mayEditMeta({ source: 'upstream:(removed)' }, true)).toBe(false)
    expect(mayEditMeta(undefined, true)).toBe(false)
  })
  it('counts a summary as the server does: code points after NFC', () => {
    expect(codePoints('Cafe' + String.fromCharCode(0x301))).toBe(4)
    expect(codePoints('\u{1F30D}'.repeat(160))).toBe(160)
  })
  it('renders a summary as text, never as markup', () => {
    const { container } = render(<EntrySummary summary={'<b>bold</b> <a href="https://evil.example">x</a>'} />)
    expect(container.querySelector('b')).toBeNull()
    expect(container.querySelector('a')).toBeNull()
    expect(container.textContent).toContain('<b>bold</b>')
    expect(container.querySelector('bdi')).not.toBeNull() // its direction isolated
  })
  it('saves a changed summary with the icon kept by its hash, and a removed icon as none', async () => {
    const onSave = vi.fn()
    const icon = { sha256: 'cd'.repeat(32), width: 64, height: 64 }
    const { rerender } = render(<MetaDialog open={false} name="geo" summary="Old" icon={icon} onSave={onSave} onClose={() => {}} />)
    rerender(<MetaDialog open name="geo" summary="Old" icon={icon} onSave={onSave} onClose={() => {}} />)
    fireEvent.change(screen.getByDisplayValue('Old'), { target: { value: 'New' } })
    fireEvent.click(screen.getByText('Save'))
    await waitFor(() => expect(onSave).toHaveBeenCalledWith({ summary: 'New', icon: { sha256: icon.sha256 } }))
    fireEvent.click(screen.getByLabelText(/remove the icon/))
    fireEvent.click(screen.getByText('Save'))
    await waitFor(() => expect(onSave).toHaveBeenLastCalledWith({ summary: 'New', icon: null }))
  })
})
