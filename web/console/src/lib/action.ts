import { useCallback, useRef, useState } from 'react'
import { ApiError } from './http'

/** A form's or a button's call (console phase 2b): one at a time, its failure kept for the page, a
 * 412 worded as "it changed" (then onStale reads it again). act answers whether the call went
 * through. */
export function useAction(what = 'It', onStale?: () => void) {
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const running = useRef(false)
  const act = useCallback(async (fn: () => Promise<unknown>): Promise<boolean> => {
    if (running.current) return false
    running.current = true
    setBusy(true)
    setFailure(null)
    try {
      await fn()
      return true
    } catch (e) {
      if (e instanceof ApiError && e.status === 412) {
        setFailure(new Error(`${what} changed meanwhile: it was read again.`))
        onStale?.()
      } else setFailure(e)
      return false
    } finally {
      running.current = false
      setBusy(false)
    }
  }, [what, onStale])
  return { busy, failure, act, setFailure }
}
