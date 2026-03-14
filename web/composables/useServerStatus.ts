import { ref } from 'vue'

const isOnline = ref(false)
const lastChecked = ref<Date | null>(null)

export const useServerStatus = () => {
  const setOnline = (value: boolean) => {
    isOnline.value = value
    lastChecked.value = new Date()
  }

  return {
    isOnline,
    lastChecked,
    setOnline,
  }
}
