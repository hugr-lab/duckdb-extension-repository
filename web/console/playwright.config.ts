import { defineConfig, devices } from '@playwright/test'

// The console end to end (spec 0015), against kista serve and Keycloak that e2e/run.sh starts; it
// sets E2E_* (the URLs, this run's passwords). Chromium only: the console is a desktop app.
export default defineConfig({
  testDir: 'e2e',
  timeout: 60_000,
  retries: 0,
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL: process.env.E2E_KISTA_URL,
    ignoreHTTPSErrors: true, // the run's own self-signed certificate
    // no trace: it would record this run's passwords into a CI artifact
    trace: 'off',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1440, height: 900 },
        launchOptions: { args: ['--host-resolver-rules=MAP kista.test 127.0.0.1'] },
      },
    },
  ],
})
