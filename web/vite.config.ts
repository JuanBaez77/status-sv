import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

// El panel se compila a estáticos y se embebe en el binario del control plane
// (docs/DISENO.md), así que en producción no hay Node corriendo.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  build: {
    // El tope del bundle (300 KB comprimido) se verifica en CI con
    // scripts/check-size.mjs. Los mapas de fuente quedan afuera del bundle que
    // se mide, pero se publican para poder leer un error de producción.
    sourcemap: true,
  },
  server: {
    // En desarrollo el front corre en 5173 y la API en 8080: el proxy evita
    // CORS y hace que la cookie de sesión viaje como en producción, donde los
    // dos están en el mismo origen.
    proxy: {
      '/v1': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false,
        ws: true,
      },
    },
  },
  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/test/setup.ts'],
    css: false,
  },
})
