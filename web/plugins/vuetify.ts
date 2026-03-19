import { createVuetify } from 'vuetify'
import * as components from 'vuetify/components'
import * as directives from 'vuetify/directives'

export default defineNuxtPlugin((app) => {
  const vuetify = createVuetify({
    components,
    directives,
    theme: {
      defaultTheme: 'boltqLight',
      themes: {
        boltqLight: {
          dark: false,
          colors: {
            background: '#f8f9fb',
            surface: '#ffffff',
            'surface-variant': '#f1f3f5',
            primary: '#6366f1',
            secondary: '#8b5cf6',
            accent: '#06b6d4',
            success: '#10b981',
            warning: '#f59e0b',
            error: '#ef4444',
            info: '#3b82f6',
            'on-background': '#111827',
            'on-surface': '#374151',
          },
        },
        boltqDark: {
          dark: true,
          colors: {
            background: '#0f1117',
            surface: '#1a1d27',
            'surface-variant': '#242836',
            primary: '#818cf8',
            secondary: '#a78bfa',
            accent: '#22d3ee',
            success: '#34d399',
            warning: '#fbbf24',
            error: '#f87171',
            info: '#60a5fa',
            'on-background': '#f1f5f9',
            'on-surface': '#e2e8f0',
          },
        },
      },
    },
    defaults: {
      VCard: {
        rounded: 'xl',
        elevation: 0,
      },
      VBtn: {
        rounded: 'lg',
      },
      VChip: {
        rounded: 'lg',
      },
      VTextField: {
        variant: 'outlined',
        density: 'compact',
        color: 'primary',
      },
    },
  })
  app.vueApp.use(vuetify)
})
