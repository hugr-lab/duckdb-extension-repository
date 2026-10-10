/// <reference types="vitest" />
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The standalone console (spec 0015): relative asset URLs under <base href="/ui/">, no inline
// script (the module-preload polyfill is one) and no data: fonts, for the CSP.
export default defineConfig({
  plugins: [react()],
  base: './',
  build: {
    outDir: 'dist/app',
    emptyOutDir: true,
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
  },
  server: {
    // npm run dev against a local kista (KISTA_URL, default https://localhost:8443)
    proxy: { '/api': { target: process.env.KISTA_URL ?? 'https://localhost:8443', secure: false } },
  },
  test: { environment: 'jsdom', include: ['src/**/*.test.{ts,tsx}'] },
})
