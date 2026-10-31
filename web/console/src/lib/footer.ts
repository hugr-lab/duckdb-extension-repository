// A DuckDB extension file's footer, read in the browser (kista's internal/extfile): the body ends with
// a 256-byte metadata block, then a 256-byte signature. The block is eight 32-byte zero-padded
// fields stored in reverse order: field 0 the magic "4", 1 the platform, 2 the DuckDB version (or C
// API version), 3 the extension version, 4 the ABI. kista reads it again and decides; this only
// fills a form.

export const footerSize = 512

export interface Footer {
  platform: string
  version: string
  /** the DuckDB version, or the minimum C API version for a C_STRUCT extension */
  duckdb: string
  abi: string
}

const field = /^[A-Za-z0-9._-]{1,32}$/

/** The metadata of a file's last 512 bytes, or null when they are not an extension's footer. */
export function parseFooter(tail: Uint8Array): Footer | null {
  if (tail.length < footerSize) return null
  const block = tail.subarray(tail.length - footerSize, tail.length - 256)
  const fields: string[] = []
  for (let i = 0; i < 8; i++) {
    const raw = block.subarray((7 - i) * 32, (8 - i) * 32)
    const end = raw.indexOf(0)
    fields.push(new TextDecoder().decode(end < 0 ? raw : raw.subarray(0, end)))
  }
  if (fields[0] !== '4' || !field.test(fields[1]) || !field.test(fields[3])) return null
  return { platform: fields[1], duckdb: fields[2], version: fields[3], abi: fields[4] || 'CPP' }
}

/** An extension's name from its file name: "geo.duckdb_extension" is geo; null for another name. */
export function nameFromFile(file: string) {
  const m = /^([a-z0-9_]{1,64})\.duckdb_extension$/.exec(file)
  return m ? m[1] : null
}

/** A file's footer: its last 512 bytes, parsed. */
export async function readFooter(f: Blob): Promise<Footer | null> {
  if (f.size < footerSize) return null
  return parseFooter(new Uint8Array(await f.slice(f.size - footerSize).arrayBuffer()))
}
