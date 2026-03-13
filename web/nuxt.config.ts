import vuetify, { transformAssetUrls } from 'vite-plugin-vuetify'

export default defineNuxtConfig({
  compatibilityDate: '2025-03-13',

  build: {
    transpile: ['vuetify'],
  },

  modules: [
    (_options, nuxt) => {
      nuxt.hooks.hook('vite:extendConfig', (config) => {
        config.plugins!.push(vuetify({ autoImport: true }))
      })
    },
  ],

  vite: {
    vue: {
      template: {
        transformAssetUrls,
      },
    },
  },

  css: [
    'vuetify/styles',
    '@mdi/font/css/materialdesignicons.css',
    '~/assets/styles/main.scss',
  ],

  runtimeConfig: {
    boltqUrl: process.env.BOLTQ_ADMIN_URL || 'http://localhost:9090',
  },

  app: {
    head: {
      title: 'BoltQ Admin',
      meta: [
        { name: 'description', content: 'BoltQ Message Queue Admin Dashboard' },
      ],
    },
  },
})
