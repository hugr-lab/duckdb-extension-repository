import type { ReactNode } from 'react'
import { LogOut, Package } from 'lucide-react'

/** The standalone console's frame: the header with the environment badge and the signed-in user. */
export function Shell(props: { environment?: string; where?: ReactNode; user?: string; onSignOut?: () => void; children: ReactNode }) {
  return (
    <div className="min-h-screen min-w-[960px]">
      <header className="flex items-center gap-4 border-b border-line bg-surface px-6 py-3">
        <a href="/ui/" className="flex items-center gap-2 text-lg font-extrabold text-ink">
          <Package className="h-5 w-5 text-brand" /> kista
        </a>
        {props.environment && <span className="rounded-full bg-warning-soft px-2.5 py-0.5 text-xs font-bold text-warning">{props.environment}</span>}
        <div className="text-sm text-ink-muted">{props.where}</div>
        {props.user && (
          <div className="ml-auto flex items-center gap-3 text-sm">
            <span className="font-mono text-xs">{props.user}</span>
            <button onClick={props.onSignOut} className="flex items-center gap-1 rounded-full px-3 py-1 hover:bg-row" title="Sign out">
              <LogOut className="h-4 w-4" /> Sign out
            </button>
          </div>
        )}
      </header>
      <main className="mx-auto max-w-6xl p-6">{props.children}</main>
    </div>
  )
}
