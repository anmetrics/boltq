import { createVuetify } from 'vuetify'
import * as components from 'vuetify/components'
import * as directives from 'vuetify/directives'

export default defineNuxtPlugin((app) => {
  const vuetify = createVuetify({
    components,
    directives,
    theme: {
      defaultTheme: 'light',
      themes: {
        light: {
          colors: {
            primary: '#2D9CDB',
            secondary: '#6C757D',
            accent: '#27AE60',
            success: '#27AE60',
            info: '#2D9CDB',
            warning: '#F2994A',
            error: '#EB5757',
            background: '#F8F9FA',
            surface: '#FFFFFF',
            'on-surface': '#1A1A2E',
            'sidebar-bg': '#F0F4F8',
          },
        },
      },
    },
    defaults: {
      VBtn: {
        variant: 'flat',
        rounded: 'lg',
      },
      VCard: {
        rounded: 'lg',
        elevation: 0,
      },
      VTextField: {
        variant: 'outlined',
        density: 'compact',
      },
      VChip: {
        rounded: 'lg',
      },
    },
  })
  app.vueApp.use(vuetify)
})
