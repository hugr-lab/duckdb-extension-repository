// A token's freshness (spec 0015): read from its claims, never verified here (the API does).

/** When a token must be renewed, in ms: a minute before it expires, or five minutes before the
 * API's freshness limit (iat + maxAge), whichever is first. */
export function renewAt(iat: number | undefined, exp: number | undefined, maxAgeSeconds: number): number {
  const limits: number[] = []
  if (exp) limits.push((exp - 60) * 1000)
  // the margin is at most half the limit, so a short one does not renew on every request
  if (iat && maxAgeSeconds > 0) limits.push((iat + maxAgeSeconds - Math.min(300, maxAgeSeconds / 2)) * 1000)
  return limits.length ? Math.min(...limits) : Infinity
}

export function claims(token: string): { iat?: number; exp?: number } {
  try {
    const part = token.split('.')[1].replace(/-/g, '+').replace(/_/g, '/')
    return JSON.parse(atob(part.padEnd(part.length + ((4 - (part.length % 4)) % 4), '=')))
  } catch {
    return {}
  }
}
