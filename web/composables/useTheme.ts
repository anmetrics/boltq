import { useTheme as useVuetifyTheme } from 'vuetify'

const isDark = ref(false)

export const useAppTheme = () => {
  const toggleTheme = () => {
    isDark.value = !isDark.value
    applyTheme()
  }

  const applyTheme = () => {
    if (import.meta.client) {
      const vuetifyTheme = useVuetifyTheme()
      vuetifyTheme.global.name.value = isDark.value ? 'boltqDark' : 'boltqLight'
      document.documentElement.setAttribute('data-theme', isDark.value ? 'dark' : 'light')
      localStorage.setItem('boltq-theme', isDark.value ? 'dark' : 'light')
    }
  }

  const initTheme = () => {
    if (import.meta.client) {
      const saved = localStorage.getItem('boltq-theme')
      if (saved === 'dark') {
        isDark.value = true
      } else if (!saved) {
        isDark.value = window.matchMedia('(prefers-color-scheme: dark)').matches
      }
      applyTheme()
    }
  }

  return {
    isDark,
    toggleTheme,
    initTheme,
  }
}
