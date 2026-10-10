import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The micro-frontend (spec 0015 phase 1b): one ES module, dist/mfe/kista.js, exporting contract,
// mountKista and KistaConsole and defining <kista-console>; served at /ui/mfe/ (CORS for
// ui.allowed_origins). Its styles travel inside it, for its shadow root; its fonts and images are
// files beside it, named relative to the module.
export default defineConfig({
  plugins: [react()],
  base: './',
  publicDir: false,
  define: { 'process.env.NODE_ENV': JSON.stringify('production') },
  build: {
    outDir: 'dist/mfe',
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false,
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    rollupOptions: {
      input: 'src/mfe.tsx',
      preserveEntrySignatures: 'strict',
      output: { format: 'es', entryFileNames: 'kista.js', inlineDynamicImports: true },
    },
  },
})
