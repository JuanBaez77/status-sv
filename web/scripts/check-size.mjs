// Verifica la meta de tamaño del panel: 300 KB comprimido (docs/DISENO.md).
//
// Se mide lo que el navegador descarga en la primera carga: el HTML, el CSS y
// el JavaScript de dist/, comprimidos con gzip. Los mapas de fuente no cuentan,
// porque solo se bajan si alguien abre las herramientas del navegador.
import { gzipSync } from 'node:zlib'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, extname, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const MAX_BYTES = 300 * 1024
const COUNTED = new Set(['.html', '.css', '.js'])

// fileURLToPath y no .pathname: en Windows .pathname deja "/C:/…", que no es
// una ruta que el sistema entienda.
const dist = fileURLToPath(new URL('../dist/', import.meta.url))

/** walk devuelve todos los archivos de un directorio, en cualquier nivel. */
function walk(dir) {
  return readdirSync(dir).flatMap((entry) => {
    const full = join(dir, entry)
    return statSync(full).isDirectory() ? walk(full) : [full]
  })
}

let files
try {
  files = walk(dist)
} catch {
  console.error('No hay dist/: correr "npm run build" antes que esto.')
  process.exit(1)
}

let total = 0
const rows = []
for (const file of files) {
  if (!COUNTED.has(extname(file))) continue
  const gzipped = gzipSync(readFileSync(file)).length
  total += gzipped
  rows.push({ archivo: relative(dist, file), gzip: `${(gzipped / 1024).toFixed(1)} KB` })
}

console.table(rows)
const totalKiB = (total / 1024).toFixed(1)
const maxKiB = (MAX_BYTES / 1024).toFixed(0)

if (total > MAX_BYTES) {
  console.error(`El panel pesa ${totalKiB} KB comprimido y la meta es ${maxKiB} KB.`)
  process.exit(1)
}
console.log(`Panel: ${totalKiB} KB comprimido (meta ${maxKiB} KB).`)
