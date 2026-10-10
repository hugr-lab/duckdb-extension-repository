import { defineConfig } from 'vite'

// The micro-frontend's test host (mfe-host/, spec 0015 phase 1b): `npm run host` serves a page on
// another origin than kista that mounts the console as a platform's shell would.
export default defineConfig({
  root: 'mfe-host',
  server: { port: Number(process.env.HOST_PORT ?? 18444), strictPort: true, host: '127.0.0.1' },
  appType: 'spa',
})
