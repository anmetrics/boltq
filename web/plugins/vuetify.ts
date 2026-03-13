import { createVuetify } from 'vuetify'
import * as components from 'vuetify/components'
import * as directives from 'vuetify/directives'

export default defineNuxtPlugin((app) => {
  const vuetify = createVuetify({
    components,
    directives,
    theme: {
      defaultTheme: 'boltqDark',
      themes: {
        boltqDark: {
          dark: true,
          colors: {
            background: '#0f1923',
            surface: '#1a2735',
            'surface-variant': '#1e3040',
            primary: '#00d4aa',
            secondary: '#4fc3f7',
            accent: '#7c4dff',
            success: '#66bb6a',
            warning: '#ffa726',
            error: '#ef5350',
            info: '#42a5f5',
            'on-background': '#e0e6ed',
            'on-surface': '#c8d6e5',
          },
        },
      },
    },
    defaults: {
      VCard: {
        rounded: 'lg',
        elevation: 0,
      },
      VBtn: {
        rounded: 'lg',
      },
      VChip: {
        rounded: 'lg',
      },
    },
  })
  app.vueApp.use(vuetify)
})
