import { type ReactNode, useEffect, useRef } from 'react'
import { AlertTriangle, ChevronLeft, ChevronRight, Loader2, Search } from 'lucide-react'

// The console's parts, in the Hugr Lab design system as tresor-server's console draws it: pill
// buttons, chips, tables in a bordered box, no shadows.

type Tone = 'brand' | 'outline' | 'plain' | 'danger' | 'danger-outline'

export function Button(props: {
  children: ReactNode
  onClick?: () => void
  tone?: Tone
  disabled?: boolean
  type?: 'button' | 'submit'
  title?: string
  /** The width of its container, the label centred. */
  wide?: boolean
}) {
  const tone = {
    brand: 'border border-brand-strong bg-brand-strong text-brand-on',
    outline: 'border border-brand-strong bg-transparent text-brand-strong',
    plain: 'border border-line bg-surface text-ink hover:bg-surface-soft',
    danger: 'border border-danger bg-danger text-white',
    'danger-outline': 'border border-danger bg-transparent text-danger',
  }[props.tone ?? 'plain']
  return (
    <button
      type={props.type ?? 'button'}
      onClick={props.onClick}
      disabled={props.disabled}
      title={props.title}
      className={`inline-flex items-center gap-2 rounded-full px-4 py-2 text-[14px] font-semibold leading-5 transition-colors disabled:cursor-not-allowed disabled:opacity-50 ${props.wide ? 'justify-center py-3' : ''} ${tone}`}
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
  public: 'bg-surface-soft text-ink-muted',
  private: 'border border-line text-ink',
  mixed: 'border border-line text-ink',
}

export function Badge({ children }: { children: string }) {
  return <span className={`chip ${badgeTone[children] ?? 'bg-surface-soft text-ink'}`}>{children}</span>
}

/** A page's title: an eyebrow (where it is), the title, and a line of context or actions beside. */
export function PageTitle({ eyebrow, title, children }: { eyebrow?: ReactNode; title: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-end gap-4">
      <div className="flex flex-col gap-0.5">
        {eyebrow && <span className="eyebrow">{eyebrow}</span>}
        <h1 className="m-0 text-[24px] font-bold leading-8 tracking-[-0.01em]">{title}</h1>
      </div>
      {children}
    </div>
  )
}

export function Card({ title, actions, children }: { title?: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="rounded-lg border border-line bg-surface p-6">
      {(title || actions) && (
        <div className="mb-4 flex items-center justify-between gap-4">
          <h2 className="m-0 text-[18px] font-bold leading-[26px]">{title}</h2>
          <div className="flex gap-2">{actions}</div>
        </div>
      )}
      {children}
    </section>
  )
}

export function Loading() {
  return (
    <div className="flex items-center gap-2 p-6 text-ink-muted" aria-busy="true">
      <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> Loading…
    </div>
  )
}

export function ErrorBox({ error }: { error: unknown }) {
  const msg = error instanceof Error ? error.message : String(error)
  return (
    <div role="alert" className="flex items-start gap-2.5 rounded-md bg-danger-soft px-3.5 py-3 text-danger">
      <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" /> <span>{msg}</span>
    </div>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return <p className="p-6 text-center text-ink-muted">{children}</p>
}

export function Mono({ children }: { children: ReactNode }) {
  return <span className="break-all font-mono text-[13px]">{children}</span>
}

/** A table of rows; columns are [header, cell renderer]. */
export function Table<T>({ rows, columns, rowKey }: { rows: T[]; columns: [string, (r: T) => ReactNode][]; rowKey: (r: T) => string }) {
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="w-full border-collapse">
        <thead className="bg-surface-soft">
          <tr>
            {columns.map(([h], i) => (
              <th key={h} className={`th ${i === 0 ? 'pl-4' : ''}`}>
                {h}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => (
            <tr key={rowKey(r)} className="border-t border-line hover:bg-row">
              {columns.map(([h, cell], i) => (
                <td key={h} className={`td ${i === 0 ? 'pl-4' : ''}`}>
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
    <dialog ref={ref} onClose={props.onClose} className="w-[30rem] rounded-lg border border-line bg-surface p-6 text-ink">
      <h3 className="m-0 mb-3 text-[20px] font-bold leading-7">{props.title}</h3>
      <div className="mb-5 space-y-3">{props.children}</div>
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

/** A name-prefix search, answered by the API (spec 0015 phase 1b): never a filter of one page. */
export function SearchBox({ value, onChange, label, placeholder = 'Name starts with…' }: { value: string; onChange: (v: string) => void; label: string; placeholder?: string }) {
  return (
    <label className="flex w-[300px] items-center gap-2 rounded-full border border-line px-3.5 py-1.5 text-ink-muted">
      <Search size={15} aria-hidden />
      <span className="sr-only">{label}</span>
      <input type="search" value={value} placeholder={placeholder} aria-label={label} maxLength={64}
        onChange={(e) => onChange(e.target.value.replace(/[^A-Za-z0-9_.-]/g, ''))}
        className="min-w-0 flex-1 border-0 bg-transparent text-[13px] text-ink outline-none" />
    </label>
  )
}

/** A choice among a few values, as pills (tresor-server's filter chips). */
export function Chips<T extends string>({ value, options, onChange, label }: { value: T; options: [T, string][]; onChange: (v: T) => void; label: string }) {
  return (
    <div role="group" aria-label={label} className="flex flex-wrap gap-1.5">
      {options.map(([v, text]) => (
        <button key={v} type="button" aria-pressed={v === value} onClick={() => onChange(v)}
          className={`rounded-full px-3.5 py-1.5 text-[13px] ${v === value ? 'border border-brand-strong bg-brand-strong font-semibold text-brand-on' : 'border border-line bg-surface text-ink'}`}>
          {text}
        </button>
      ))}
    </div>
  )
}

/** Pages by the API's cursors: the size, where we are, previous and next. */
export function Pager({ page, size, count, hasNext, onPrev, onNext, onSize }: {
  page: number; size: number; count: number; hasNext: boolean; onPrev: () => void; onNext: () => void; onSize: (n: number) => void
}) {
  const from = count ? page * size + 1 : 0
  return (
    <nav aria-label="Pages" className="flex flex-wrap items-center gap-3 text-[13px] text-ink-muted">
      <label className="flex items-center gap-2">
        Rows per page
        <select value={size} onChange={(e) => onSize(Number(e.target.value))} className="field py-1 text-[13px]">
          {[50, 100, 200].map((n) => <option key={n} value={n}>{n}</option>)}
        </select>
      </label>
      <span className="ml-auto">{count ? `${from}–${from + count - 1}` : 'Nothing here'}</span>
      <button type="button" aria-label="Previous page" disabled={page === 0} onClick={onPrev}
        className="inline-flex h-9 w-9 items-center justify-center rounded-full border border-line bg-surface text-ink disabled:opacity-40">
        <ChevronLeft size={16} aria-hidden />
      </button>
      <button type="button" aria-label="Next page" disabled={!hasNext} onClick={onNext}
        className="inline-flex h-9 w-9 items-center justify-center rounded-full border border-line bg-surface text-ink disabled:opacity-40">
        <ChevronRight size={16} aria-hidden />
      </button>
    </nav>
  )
}
