import '@fontsource/manrope/400.css'
import '@fontsource/manrope/600.css'
import '@fontsource/manrope/800.css'
import '@fontsource/jetbrains-mono/400.css'
import './styles.css'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'

if (window.matchMedia?.('(prefers-color-scheme: dark)').matches) {
  document.documentElement.dataset.kistaTheme = 'dark'
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
