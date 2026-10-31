import { useState, type ReactNode } from 'react'
import { Button, Card, ErrorBox, Note } from '../components/ui'
import { useTenant } from '../lib/context'
import { useAction } from '../lib/action'
import { holdInput, holdWords } from './Sources'

// A hold (spec 0033, 0037): what a source brings waits this long in the catalogue before a channel's
// automatic placement takes it. One form for every hold the console sets: a channel's, a publisher's,
// an eager mirror's, and the tenant's minimum for distributors' publishers; each PUTs {"hold": "24h"}
// to its …/hold route ("0" clears it; days are sent as hours).

export interface HoldFormProps {
  /** the …/hold route */
  path: string
  /** the form's accessible name ("publisher hold") */
  label: string
  /** the hold as the API answers it (omitted for none); null while it is read */
  current?: string | null
  /** the words before the hold ("Hold", "Minimum hold") */
  caption?: string
  /** a sentence after the hold, in the form's line */
  inline?: ReactNode
  /** the input's accessible name and the button's text */
  input?: string
  button?: string
  /** the owner's ETag, sent as If-Match (a 412 reads it again) */
  ifMatch?: string
  /** what a 412 says changed ("The upstream"); default "The hold" */
  owner?: string
  /** a card around it with this title; none: the form alone */
  card?: string
  note?: ReactNode
  onChanged: () => void
}

export function HoldForm(p: HoldFormProps) {
  const { client } = useTenant()
  const [value, setValue] = useState('')
  const { busy, failure, act } = useAction(p.owner ?? 'The hold', p.onChanged)
  const form = (
    <div className="flex flex-col gap-2">
      <form role="group" aria-label={p.label} className="flex flex-wrap items-center gap-2 text-[13px]"
        onSubmit={(e) => {
          e.preventDefault()
          act(() => client.put(p.path, { body: { hold: holdInput(value) }, ifMatch: p.ifMatch })).then((ok) => {
            if (ok) {
              setValue('')
              p.onChanged()
            }
          })
        }}>
        <span className="text-ink-muted">{p.caption ?? 'Hold'}</span>
        <b>{p.current === null ? '…' : holdWords(p.current)}</b>
        {p.inline && <span className="text-ink-muted">{p.inline}</span>}
        <input aria-label={p.input ?? 'new hold'} className="field w-32 py-1 font-mono text-[13px]" placeholder="24h, 2d, 0" required value={value}
          onChange={(e) => setValue(e.target.value)} />
        <Button type="submit" disabled={busy}>{p.button ?? 'Set hold'}</Button>
      </form>
      {failure != null && <ErrorBox error={failure} />}
      {p.note && <Note>{p.note}</Note>}
    </div>
  )
  return p.card ? <Card title={p.card}>{form}</Card> : form
}
