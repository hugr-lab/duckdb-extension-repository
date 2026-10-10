// The console's frame, tresor-server's (spec 0015 phase 1b): standalone, a navy sidebar with the
// sections and a top bar with the environment, the theme and the user; embedded in a host's shell,
// only the sections, as tabs above the content.
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { NavLink } from 'react-router-dom'
import { ChevronDown, LogOut, Moon, Sun } from 'lucide-react'
import iconOnDark from '../assets/hugr-icon-on-dark.svg'
import logo from '../assets/hugr-logo.svg'
import logoOnDark from '../assets/hugr-logo-on-dark.svg'
import { useFrame } from '../lib/frame'

const themeKey = 'kista.theme'

/** The standalone page's theme: the one chosen here before, else the system's. */
export function initialTheme(): 'light' | 'dark' {
  try {
    const t = localStorage.getItem(themeKey)
    if (t === 'light' || t === 'dark') return t
  } catch {
    /* storage refused */
  }
  return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light'
}

function useTheme(): ['light' | 'dark', (t: 'light' | 'dark') => void] {
  const [theme, set] = useState<'light' | 'dark'>(() => (document.documentElement.dataset.kistaTheme === 'dark' ? 'dark' : 'light'))
  return [
    theme,
    (t) => {
      document.documentElement.dataset.kistaTheme = t
      try {
        localStorage.setItem(themeKey, t)
      } catch {
        /* storage refused: for this page only */
      }
      set(t)
    },
  ]
}

export interface ShellProps {
  environment?: string
  /** The scope, above the sections: "Server", or the tenant's name. */
  scope?: ReactNode
  user?: string
  role?: string
  onSignOut?: () => void
  service?: { host: string; version: string }
  children: ReactNode
}

export function Shell(props: ShellProps) {
  const { embedded } = useFrame()
  if (embedded) return <EmbeddedFrame>{props.children}</EmbeddedFrame>
  return (
    <div className="kista-root flex min-h-screen">
      <Sidebar scope={props.scope} service={props.service} />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar {...props} />
        <main className="flex w-full max-w-[1360px] flex-col gap-5 px-8 pb-10 pt-7">{props.children}</main>
      </div>
    </div>
  )
}

function Sidebar({ scope, service }: { scope?: ReactNode; service?: { host: string; version: string } }) {
  const { groups } = useFrame()
  const link = ({ isActive }: { isActive: boolean }) =>
    `flex items-center gap-2.5 rounded-full px-3 py-2 no-underline ${isActive ? 'bg-[#1E4A60] font-semibold text-white' : 'text-[#C9DCE0] hover:text-white'}`
  return (
    <nav aria-label="Main" className="flex w-[232px] flex-none flex-col gap-1 bg-navy px-3.5 py-5 text-[#E8F1F2]">
      <a href="/ui/" className="flex items-center gap-2.5 px-2 pb-5 no-underline">
        <img src={iconOnDark} alt="hugr" width={28} height={28} />
        <span className="flex flex-col leading-[18px]">
          <span className="text-[17px] font-bold text-white">kista</span>
          <span className="text-[12px] text-[#9CB1BA]">extensions for DuckDB</span>
        </span>
      </a>
      {scope && <div className="mx-3 mb-1.5 text-[12px] font-semibold uppercase tracking-[0.08em] text-[#9CB1BA]">{scope}</div>}
      {groups.map((g, i) => (
        <div key={i} className="flex flex-col gap-1">
          {g.label && <div className="mx-3 mb-1.5 mt-[18px] text-[12px] font-semibold uppercase tracking-[0.08em] text-[#9CB1BA]">{g.label}</div>}
          {g.items.map((it) => (
            <NavLink key={it.to} to={it.to} end={it.end} className={link}>
              <it.icon size={18} aria-hidden />
              {it.label}
            </NavLink>
          ))}
        </div>
      ))}
      {service && (
        <div className="mt-auto flex flex-col gap-1 rounded-md bg-[#061C28] p-3">
          <span className="text-[12px] text-[#9CB1BA]">Service</span>
          <span className="truncate font-mono text-[12px]">{service.host}</span>
          <span className="text-[12px] text-[#9CB1BA]">API v1 · kista {service.version}</span>
        </div>
      )}
    </nav>
  )
}

function TopBar({ environment, user, role, onSignOut }: ShellProps) {
  const [theme, setTheme] = useTheme()
  return (
    <header className="flex items-center gap-3 border-b border-line px-8 py-3.5">
      {environment && (
        <span className="rounded-full bg-warning-soft px-2.5 py-0.5 text-[12px] font-bold uppercase tracking-[0.06em] text-warning">{environment}</span>
      )}
      <div className="ml-auto flex items-center gap-2">
        <button type="button" className="inline-flex h-9 w-9 items-center justify-center rounded-full border border-line text-ink"
          aria-label={theme === 'dark' ? 'Light theme' : 'Dark theme'} onClick={() => setTheme(theme === 'dark' ? 'light' : 'dark')}>
          {theme === 'dark' ? <Sun size={18} aria-hidden /> : <Moon size={18} aria-hidden />}
        </button>
        {user && <UserMenu user={user} role={role} onSignOut={onSignOut} />}
      </div>
    </header>
  )
}

function UserMenu({ user, role, onSignOut }: { user: string; role?: string; onSignOut?: () => void }) {
  const [open, setOpen] = useState(false)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const close = (e: MouseEvent | KeyboardEvent) => {
      if (e instanceof KeyboardEvent ? e.key === 'Escape' : !ref.current?.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', close)
    document.addEventListener('keydown', close)
    return () => {
      document.removeEventListener('mousedown', close)
      document.removeEventListener('keydown', close)
    }
  }, [open])
  const initials = user.split(/[\s._@|:-]+/).filter(Boolean).slice(0, 2).map((w) => w[0]).join('').toUpperCase()
  const name = user.length > 28 ? user.slice(0, 26) + '…' : user
  return (
    <div className="relative" ref={ref}>
      <button type="button" aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}
        className="flex items-center gap-2.5 rounded-full border border-line bg-surface py-1 pl-1 pr-3 text-ink">
        <span className="flex h-7 w-7 items-center justify-center rounded-full bg-brand-strong text-[12px] font-bold text-brand-on">{initials}</span>
        <span className="flex flex-col items-start leading-4">
          <span className="text-[13px] font-semibold">{name}</span>
          {role && <span className="text-[12px] font-semibold text-brand-strong">{role}</span>}
        </span>
        <ChevronDown size={16} aria-hidden />
      </button>
      {open && (
        <div role="menu" className="absolute right-0 top-12 z-30 flex w-[300px] flex-col gap-3 rounded-md border border-line bg-surface p-4">
          <div className="flex flex-col gap-0.5">
            <span className="eyebrow">Signed in</span>
            <span className="break-all font-mono text-[12px]">{user}</span>
          </div>
          {onSignOut && (
            <button type="button" role="menuitem" className="flex items-center gap-2 rounded-full px-2 py-1.5 text-left hover:bg-surface-soft" onClick={onSignOut}>
              <LogOut size={16} aria-hidden /> Sign out
            </button>
          )}
        </div>
      )}
    </div>
  )
}

function EmbeddedFrame({ children }: { children: ReactNode }) {
  const { groups } = useFrame()
  const tab = ({ isActive }: { isActive: boolean }) =>
    `rounded-full px-4 py-1.5 no-underline ${isActive ? 'bg-brand-strong font-semibold text-brand-on' : 'border border-line text-ink'}`
  return (
    <div className="flex flex-col gap-4">
      {groups.map((g, i) => (
        <nav key={i} aria-label={typeof g.label === 'string' ? g.label : 'kista'} className="flex flex-wrap items-center gap-1.5">
          {g.label && <span className="eyebrow mr-2">{g.label}</span>}
          {g.items.map((it) => (
            <NavLink key={it.to} to={it.to} end={it.end} className={tab}>
              {it.label}
            </NavLink>
          ))}
        </nav>
      ))}
      {children}
    </div>
  )
}

/** The standalone pages before a sign-in (the landing, the issuer choice): a card with the logo. */
export function EntryCard({ environment, children }: { environment?: string; children: ReactNode }) {
  const dark = document.documentElement.dataset.kistaTheme === 'dark'
  return (
    <div className="kista-root flex min-h-screen items-center justify-center bg-surface p-8">
      <div className="flex w-full max-w-[440px] flex-col gap-6 rounded-lg border border-line bg-surface p-10">
        <div className="flex items-center justify-between gap-3">
          <img src={dark ? logoOnDark : logo} alt="hugr" width={120} />
          {environment && (
            <span className="rounded-full bg-warning-soft px-2.5 py-0.5 text-[12px] font-bold uppercase tracking-[0.06em] text-warning">{environment}</span>
          )}
        </div>
        {children}
      </div>
    </div>
  )
}
