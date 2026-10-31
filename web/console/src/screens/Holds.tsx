import { useState } from 'react'
import { Button, Card, ErrorBox, Note } from '../components/ui'
import { useTenant } from '../lib/context'
import { useAction } from '../lib/action'
import { holdInput, holdWords } from './Sources'

// A source's hold (spec 0033): what it brings waits this long in the catalogue before any channel's
// automatic placement takes it. A publisher's and an eager mirror's hold are set with
// PUT …/{publishers|upstreams}/{name}/hold {"hold": "24h"}; "0" clears it.

/** A hold's card: the hold now and a form to set it. path is the …/hold route; current is the
 * object's hold as the API answers it (omitted for none); onChanged runs after a change. */
export function HoldForm({ path, label, current, note, onChanged }: {
  path: string
  label: string
  current?: string
  note: React.ReactNode
  onChanged: () => void
}) {
  const { client } = useTenant()
  const [value, setValue] = useState('')
  const { busy, failure, act } = useAction('The hold', onChanged)
  return (
    <Card title="Hold">
      <div className="flex flex-col gap-2">
        <form role="group" aria-label={label} className="flex flex-wrap items-center gap-2 text-[13px]"
          onSubmit={(e) => {
            e.preventDefault()
            act(() => client.put(path, { body: { hold: holdInput(value) } })).then((ok) => {
              if (ok) {
                setValue('')
                onChanged()
              }
            })
          }}>
          <span className="text-ink-muted">Hold</span>
          <b>{holdWords(current)}</b>
          <input aria-label="new hold" className="field w-32 py-1 font-mono text-[13px]" placeholder="24h, 2d, 0" required value={value}
            onChange={(e) => setValue(e.target.value)} />
          <Button type="submit" disabled={busy}>Set hold</Button>
        </form>
        {failure != null && <ErrorBox error={failure} />}
        <Note>{note}</Note>
      </div>
    </Card>
  )
}
