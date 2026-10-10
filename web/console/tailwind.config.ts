import type { Config } from 'tailwindcss'

// The Hugr Lab design system's tokens, as CSS variables a host may override (spec 0015).
const v = (name: string) => `rgb(var(--${name}) / <alpha-value>)`

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        surface: { DEFAULT: v('surface'), soft: v('surface-soft') },
        ink: { DEFAULT: v('ink'), muted: v('ink-muted') },
        line: v('border'),
        brand: { DEFAULT: v('brand'), strong: v('brand-strong'), on: v('on-brand') },
        focus: v('focus'),
        success: { DEFAULT: v('success'), soft: v('success-soft') },
        warning: { DEFAULT: v('warning'), soft: v('warning-soft') },
        danger: { DEFAULT: v('danger'), soft: v('danger-soft') },
        row: v('row'),
      },
      fontFamily: {
        sans: ['Manrope', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'monospace'],
      },
    },
  },
} satisfies Config
