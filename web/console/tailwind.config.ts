import type { Config } from 'tailwindcss'

// The Hugr Lab design system's tokens, as CSS variables a host may override (spec 0015); whole
// colours, as tresor-server's console has them.
const v = (name: string) => `var(--${name})`

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        surface: { DEFAULT: v('surface'), soft: v('surface-soft') },
        ink: { DEFAULT: v('ink'), muted: v('ink-muted') },
        line: v('border'),
        brand: { DEFAULT: v('brand'), strong: v('brand-strong'), on: v('on-brand') },
        navy: v('navy'),
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
      borderRadius: { sm: '8px', md: '16px', lg: '28px' },
    },
  },
} satisfies Config
