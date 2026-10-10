import { type ReactNode, useEffect, useRef } from 'react'
import { AlertTriangle, Loader2 } from 'lucide-react'

type Tone = 'brand' | 'plain' | 'danger'

export function Button(props: {
  children: ReactNode
  onClick?: () => void
  tone?: Tone
  disabled?: boolean
  type?: 'button' | 'submit'
  title?: string
}) {
  const tone = {
    brand: 'bg-brand text-brand-on hover:bg-brand-strong',
    plain: 'border border-line bg-surface text-ink hover:bg-row',
    danger: 'bg-danger text-white hover:opacity-90',
  }[props.tone ?? 'plain']
  return (
    <button
      type={props.type ?? 'button'}
      onClick={props.onClick}
      disabled={props.disabled}
      title={props.title}
      className={`inline-flex items-center gap-2 rounded-full px-4 py-1.5 text-sm font-semibold disabled:opacity-50 ${tone}`}
    >
      {props.children}
    </button>
  )
}

const badgeTone: Record<string, string> = {
  active: 'bg-success-soft text-success',
  deprecated: 'bg-warning-soft text-warning',
  yanked: 'bg-danger-soft text-danger',
  suspended: 'bg-danger-soft text-danger',
  ok: 'bg-success-soft text-success',
  refused: 'bg-warning-soft text-warning',
  failed: 'bg-danger-soft text-danger',
  public: 'bg-row text-ink-muted',
  private: 'bg-row text-ink',
}

export function Badge({ children }: { children: string }) {
  return (
    <span className={`rounded-full px-2.5 py-0.5 text-xs font-semibold ${badgeTone[children] ?? 'bg-row text-ink-muted'}`}>
      {children}
    </span>
  )
}

export function Card({ title, actions, children }: { title?: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="rounded-2xl border border-line bg-surface p-5">
      {(title || actions) && (
        <div className="mb-4 flex items-center justify-between gap-4">
          <h2 className="text-lg font-bold">{title}</h2>
          <div className="flex gap-2">{actions}</div>
        </div>
      )}
      {children}
    </section>
  )
}

export function Loading() {
  return (
    <div className="flex items-center gap-2 p-6 text-ink-muted">
      <Loader2 className="h-4 w-4 animate-spin" /> Loading…
    </div>
  )
}

export function ErrorBox({ error }: { error: unknown }) {
  const msg = error instanceof Error ? error.message : String(error)
  return (
    <div role="alert" className="flex items-start gap-2 rounded-xl bg-danger-soft p-4 text-danger">
      <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" /> <span>{msg}</span>
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="p-6 text-center text-ink-muted">{children}</p>
}

export function Mono({ children }: { children: ReactNode }) {
  return <span className="break-all font-mono text-xs">{children}</span>
}

/** A table of rows; columns are [header, cell renderer]. */
export function Table<T>({ rows, columns, rowKey }: { rows: T[]; columns: [string, (r: T) => ReactNode][]; rowKey: (r: T) => string }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-left text-sm">
        <thead>
          <tr className="border-b border-line text-xs uppercase tracking-wide text-ink-muted">
            {columns.map(([h]) => (
              <th key={h} className="px-3 py-2 font-semibold">
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={rowKey(r)} className="border-b border-line last:border-0 hover:bg-row">
              {columns.map(([h, cell]) => (
                <td key={h} className="px-3 py-2 align-top">
                  {cell(r)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

/** A confirmation in a native <dialog> (no injected styles: the CSP refuses them). */
export function Confirm(props: {
  open: boolean
  title: string
  children: ReactNode
  confirm: string
  danger?: boolean
  disabled?: boolean
  onConfirm: () => void
  onClose: () => void
}) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const d = ref.current
    if (!d) return
    if (props.open && !d.open) {
      d.showModal?.()
      d.querySelector<HTMLElement>('[data-autofocus]')?.focus()
    }
    if (!props.open && d.open) d.close()
  }, [props.open])
  return (
    <dialog ref={ref} onClose={props.onClose} className="w-[28rem] rounded-2xl border border-line bg-surface p-6 text-ink">
      <h3 className="mb-3 text-lg font-bold">{props.title}</h3>
      <div className="mb-5 space-y-3 text-sm">{props.children}</div>
      <div className="flex justify-end gap-2">
        <Button onClick={props.onClose}>Cancel</Button>
        <Button tone={props.danger ? 'danger' : 'brand'} disabled={props.disabled} onClick={props.onConfirm}>
          {props.confirm}
        </Button>
      </div>
    </dialog>
  )
}

export function when(s?: string) {
  if (!s) return ''
  const d = new Date(s)
  return isNaN(d.getTime()) ? s : d.toLocaleString()
}
