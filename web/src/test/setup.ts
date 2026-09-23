import '@testing-library/jest-dom/vitest'

import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

afterEach(() => {
  cleanup()
})

/**
 * jsdom no implementa ResizeObserver y los gráficos lo usan para seguir el
 * ancho de su tarjeta. Acá alcanza con que exista: en las pruebas nada cambia
 * de tamaño.
 */
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}

if (typeof globalThis.ResizeObserver !== 'function') {
  globalThis.ResizeObserver = ResizeObserverStub as unknown as typeof ResizeObserver
}

/**
 * uPlot pregunta por la densidad de la pantalla al cargarse. jsdom tampoco
 * tiene matchMedia, así que acá contesta que no, siempre.
 */
if (typeof globalThis.matchMedia !== 'function') {
  globalThis.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener() {},
    removeEventListener() {},
    addListener() {},
    removeListener() {},
    dispatchEvent: () => false,
  })) as unknown as typeof globalThis.matchMedia
}
