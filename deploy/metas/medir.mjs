// Mide las dos metas del panel que dice docs/DISENO.md, contra un control plane
// con un agent de verdad que ya está corriendo (lo levanta deploy/metas/run.sh):
//
//   Carga inicial del panel         menos de 1 s en conexión normal (Lighthouse)
//   Latencia métrica → pantalla     menos de 2 s
//
// La carga se mide con Lighthouse en el perfil de escritorio, que simula una
// conexión normal (40 ms de ida y vuelta, 10 Mbps) sin frenar la CPU, sobre el
// login y sobre la lista de servidores con la sesión abierta. La meta es el
// Largest Contentful Paint: cuándo se ve lo principal de la página.
//
// La latencia es desde la hora en que el agent tomó la muestra hasta que el
// panel la dibuja en la vista del servidor: el panel marca en data-sample-ts la
// muestra que está mostrando, y un MutationObserver anota cuándo cambia, en el
// cuadro siguiente. El agent y el navegador corren en la misma máquina, así que
// comparten el reloj.
//
// Variables: STATUSSV_URL, STATUSSV_USER, STATUSSV_PASSWORD, STATUSSV_SERVER_ID
// y CHROME_PATH (el Chrome o Chromium que se usa).
import { createHmac } from 'node:crypto'
import { appendFileSync } from 'node:fs'

import lighthouse from 'lighthouse'
import desktopConfig from 'lighthouse/core/config/desktop-config.js'
import { chromium } from 'playwright-core'

const MAX_CARGA_MS = 1000
const MAX_LATENCIA_MS = 2000
// Cuántas muestras en vivo se miden. El agent manda una cada 10 s.
const MUESTRAS = Number(process.env.MUESTRAS ?? 6)
const PUERTO_DEPURACION = 9229

const base = requerida('STATUSSV_URL').replace(/\/$/, '')
// El navegador entra por localhost: es el único origen http en el que Chrome
// acepta la cookie __Host-, que exige Secure.
const baseNavegador = (() => {
  const url = new URL(base)
  url.hostname = 'localhost'
  return url.origin
})()
const usuario = requerida('STATUSSV_USER')
const contraseña = requerida('STATUSSV_PASSWORD')
const servidor = requerida('STATUSSV_SERVER_ID')
const chrome = requerida('CHROME_PATH')

function requerida(nombre) {
  const valor = process.env[nombre]
  if (!valor) {
    console.error(`falta ${nombre}`)
    process.exit(2)
  }
  return valor
}

// --- Sesión: contraseña y alta del segundo factor, como en la prueba de punta a punta ---

/** totp calcula el código de 6 dígitos de un secreto en base32 (RFC 6238). */
function totp(secreto) {
  const alfabeto = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  let bits = ''
  for (const letra of secreto.replace(/=+$/, '')) {
    bits += alfabeto.indexOf(letra).toString(2).padStart(5, '0')
  }
  const clave = Buffer.from(bits.match(/.{8}/g).map((byte) => parseInt(byte, 2)))
  const paso = Buffer.alloc(8)
  paso.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)))
  const hmac = createHmac('sha1', clave).update(paso).digest()
  const offset = hmac[hmac.length - 1] & 0xf
  return String((hmac.readUInt32BE(offset) & 0x7fffffff) % 1_000_000).padStart(6, '0')
}

/**
 * abrirSesion hace el login desde la página, como el panel: así la cookie
 * __Host- queda en el navegador (Chrome solo la acepta por http en localhost).
 * Devuelve la cookie para pasársela a Lighthouse.
 */
async function abrirSesion(contexto) {
  const pagina = await contexto.newPage()
  await pagina.goto(`${baseNavegador}/login`)
  const pedir = (ruta, cuerpo) =>
    pagina.evaluate(
      async ([ruta, cuerpo]) => {
        const respuesta = await fetch(ruta, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: cuerpo === undefined ? '' : JSON.stringify(cuerpo),
        })
        if (!respuesta.ok) throw new Error(`${ruta} respondió ${respuesta.status}`)
        return respuesta.headers.get('Content-Type')?.includes('json') ? respuesta.json() : null
      },
      [ruta, cuerpo],
    )
  await pedir('/v1/auth/login', { username: usuario, password: contraseña })
  const alta = await pedir('/v1/auth/totp/enroll')
  await pedir('/v1/auth/totp/confirm', { code: totp(alta.secret) })
  await pagina.close()

  const cookie = (await contexto.cookies()).find((c) => c.name === '__Host-statussv_session')
  if (!cookie) throw new Error('el login no dejó la cookie de sesión')
  return `${cookie.name}=${cookie.value}`
}

// --- Carga inicial con Lighthouse ---

async function medirCarga(cookie) {
  const paginas = [
    { nombre: 'login', url: `${base}/login`, headers: {} },
    { nombre: 'lista de servidores', url: `${base}/`, headers: { Cookie: cookie } },
  ]
  const resultados = []
  for (const pagina of paginas) {
    const { lhr } = await lighthouse(
      pagina.url,
      { port: PUERTO_DEPURACION, output: 'json', logLevel: 'error', extraHeaders: pagina.headers },
      desktopConfig,
    )
    if (lhr.runtimeError) throw new Error(`Lighthouse en ${pagina.nombre}: ${lhr.runtimeError.message}`)
    // Sin la sesión, el panel manda al login: se estaría midiendo otra página.
    if (new URL(lhr.finalDisplayedUrl).pathname !== new URL(pagina.url).pathname) {
      throw new Error(`Lighthouse en ${pagina.nombre} terminó en ${lhr.finalDisplayedUrl}`)
    }
    const valor = (id) => lhr.audits[id]?.numericValue ?? NaN
    resultados.push({
      nombre: pagina.nombre,
      url: lhr.finalDisplayedUrl,
      puntaje: Math.round((lhr.categories.performance?.score ?? 0) * 100),
      fcp: valor('first-contentful-paint'),
      lcp: valor('largest-contentful-paint'),
      tbt: valor('total-blocking-time'),
    })
  }
  return resultados
}

// --- Latencia métrica → pantalla ---

async function medirLatencia(contexto) {
  const pagina = await contexto.newPage()

  await pagina.addInitScript(() => {
    window.__dibujadas = []
    new MutationObserver((cambios) => {
      for (const cambio of cambios) {
        const ts = cambio.target.getAttribute('data-sample-ts')
        if (cambio.type === 'attributes' && ts) {
          // El cuadro siguiente es cuando la muestra ya está en pantalla.
          requestAnimationFrame(() =>
            window.__dibujadas.push({ ts, dibujada: performance.timeOrigin + performance.now() }),
          )
        }
      }
    }).observe(document, { attributes: true, attributeFilter: ['data-sample-ts'], subtree: true })
  })

  await pagina.goto(`${baseNavegador}/servers/${servidor}`)
  await pagina.getByText('En vivo').waitFor({ timeout: 30_000 })
  // La primera marca sale de la API; las que siguen, del canal en vivo.
  await pagina.waitForFunction(
    (n) => window.__dibujadas.length >= n,
    MUESTRAS,
    { timeout: (MUESTRAS + 3) * 12_000, polling: 500 },
  )
  const dibujadas = await pagina.evaluate(() => window.__dibujadas)
  await pagina.close()

  const vistas = new Set()
  return dibujadas
    .filter(({ ts }) => !vistas.has(ts) && vistas.add(ts))
    .map(({ ts, dibujada }) => ({ ts, latencia: dibujada - Date.parse(ts) }))
}

// --- Resultado ---

const ms = (n) => `${Math.round(n)} ms`

async function main() {
  const navegador = await chromium.launch({
    executablePath: chrome,
    // Lighthouse se conecta al mismo Chrome por el puerto de depuración.
    args: [`--remote-debugging-port=${PUERTO_DEPURACION}`],
  })

  let carga, latencias
  try {
    const contexto = await navegador.newContext()
    const cookie = await abrirSesion(contexto)
    carga = await medirCarga(cookie)
    latencias = await medirLatencia(contexto)
  } finally {
    await navegador.close()
  }

  const fallas = []
  const lineas = ['## Metas del panel', '', '| Página | Puntaje | FCP | LCP | TBT |', '| --- | --- | --- | --- | --- |']
  for (const r of carga) {
    lineas.push(`| ${r.nombre} | ${r.puntaje} | ${ms(r.fcp)} | ${ms(r.lcp)} | ${ms(r.tbt)} |`)
    if (!(r.lcp < MAX_CARGA_MS)) fallas.push(`la carga de ${r.nombre} tardó ${ms(r.lcp)} (meta: menos de ${ms(MAX_CARGA_MS)})`)
  }

  // La primera es la muestra que trajo la API al abrir la vista: no mide el
  // canal en vivo.
  const enVivo = latencias.slice(1)
  if (enVivo.length === 0) fallas.push('no llegó ninguna muestra en vivo')
  const maxima = Math.max(...enVivo.map((l) => l.latencia))
  const ordenadas = enVivo.map((l) => l.latencia).sort((a, b) => a - b)
  const mediana = ordenadas[Math.floor(ordenadas.length / 2)]
  lineas.push('', `Latencia métrica → pantalla, ${enVivo.length} muestras en vivo: mediana ${ms(mediana)}, máxima ${ms(maxima)} (meta: menos de ${ms(MAX_LATENCIA_MS)}).`, '')
  for (const l of enVivo) lineas.push(`- ${l.ts}: ${ms(l.latencia)}`)
  if (enVivo.some((l) => l.latencia < 0)) fallas.push('hay una latencia negativa: los relojes no coinciden')
  if (!(maxima < MAX_LATENCIA_MS)) fallas.push(`una muestra tardó ${ms(maxima)} en verse (meta: menos de ${ms(MAX_LATENCIA_MS)})`)

  const resumen = lineas.join('\n')
  console.log(resumen)
  if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, resumen + '\n')

  if (fallas.length > 0) {
    for (const falla of fallas) console.error(`FALLA: ${falla}`)
    process.exit(1)
  }
  console.log('\nlas dos metas se cumplen')
}

await main()
