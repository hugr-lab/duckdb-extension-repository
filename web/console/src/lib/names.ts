/** Names typed into a field: separated by commas or spaces, each once. */
export function entryNames(s: string): string[] {
  return [...new Set(s.split(/[\s,]+/).filter(Boolean))]
}
