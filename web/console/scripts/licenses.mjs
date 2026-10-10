// The licence gate (spec 0015): every package the console ships (production dependencies, with
// theirs) must be under a licence kista may bundle.
import { execFileSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'

const allowed = new Set(['MIT', 'ISC', 'Apache-2.0', 'BSD-2-Clause', 'BSD-3-Clause', '0BSD', 'OFL-1.1'])

const tree = JSON.parse(execFileSync('npm', ['ls', '--omit=dev', '--all', '--json', '--long'], { encoding: 'utf8' }))
const bad = []
const seen = new Set()
const walk = (deps = {}) => {
  for (const [name, d] of Object.entries(deps)) {
    const id = `${name}@${d.version}`
    if (seen.has(id)) continue
    seen.add(id)
    let lic = d.license
    if (!lic && d.path) {
      try {
        lic = JSON.parse(readFileSync(join(d.path, 'package.json'), 'utf8')).license
      } catch {
        /* no package.json */
      }
    }
    const ids = String(lic ?? 'UNKNOWN').replace(/[()]/g, '').split(/\s+OR\s+/)
    if (!ids.some((l) => allowed.has(l.trim()))) bad.push(`${id}: ${lic ?? 'no licence'}`)
    walk(d.dependencies)
  }
}
walk(tree.dependencies)
if (bad.length) {
  console.error('packages under licences the console may not ship:\n  ' + bad.join('\n  '))
  process.exit(1)
}
console.log(`${seen.size} packages, all under allowed licences`)
