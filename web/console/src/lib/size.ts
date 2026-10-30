/** A size in words: 1.5 GiB, 16 MiB, 512 B. */
export function sizeText(n: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return i === 0 ? `${n} B` : `${v >= 10 ? Math.round(v) : Math.round(v * 10) / 10} ${units[i]}`
}
