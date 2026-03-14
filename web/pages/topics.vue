<template>
  <div class="obsidian-page">
    <div class="d-flex align-center mb-8">
      <div>
        <h1 class="text-h4 font-weight-black gradient-text">Global Topics</h1>
        <p class="text-caption text-grey mt-1">Active pub/sub channels and broadcast fan-out</p>
      </div>
      <v-spacer />
      <v-btn icon="mdi-refresh" variant="text" size="small" :loading="loading" @click="refresh" class="neon-btn" />
    </div>

    <div class="glass-card table-card overflow-hidden">
      <v-table density="compact" class="obsidian-table">
        <thead>
          <tr>
            <th class="text-left">Topic Name</th>
            <th class="text-right">Fans (Subscribers)</th>
            <th class="text-right">State</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="rows.length === 0">
            <td colspan="3" class="text-center pa-12">
              <v-icon size="64" color="grey-darken-3" class="mb-4">mdi-broadcast-off</v-icon>
              <div class="text-h6 text-grey-darken-1">No topics established</div>
              <p class="text-caption text-grey-darken-2 mt-1">Subscribe via TCP to initialize a topic</p>
            </td>
          </tr>
          <tr v-for="t in rows" :key="t.name">
            <td class="mono py-4">
              <div class="d-flex align-center">
                <v-icon size="14" class="mr-2 text-green">mdi-broadcast</v-icon>
                <span class="text-green-accent-1">{{ t.name }}</span>
              </div>
            </td>
            <td class="text-right">
              <v-chip :color="t.subscribers > 0 ? 'green' : 'grey-darken-3'" size="x-small" variant="tonal" class="font-weight-bold">
                {{ t.subscribers }} subs
              </v-chip>
            </td>
            <td class="text-right">
              <div class="d-flex align-center justify-end">
                <div class="status-indicator mr-2" :class="{ online: t.subscribers > 0 }" />
                <span class="text-caption font-weight-bold" :class="t.subscribers > 0 ? 'text-green' : 'text-grey'">
                  {{ t.subscribers > 0 ? 'ACTIVE' : 'IDLE' }}
                </span>
              </div>
            </td>
          </tr>
        </tbody>
      </v-table>
      <div class="footer-stats d-flex align-center px-4 py-3">
        <v-icon size="14" class="mr-2 text-grey">mdi-information-outline</v-icon>
        <span class="text-caption text-grey">Discovered Topics: {{ rows.length }}</span>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
const api = useApi()
const loading = ref(false)
const stats = ref<any>(null)

const rows = computed(() => {
  const topics = stats.value?.Topics || {}
  return Object.entries(topics).map(([name, subscribers]) => ({
    name,
    subscribers: subscribers as number,
  })).sort((a, b) => a.name.localeCompare(b.name))
})

async function refresh() {
  loading.value = true
  try { stats.value = await api.getStats() } catch {}
  loading.value = false
}

let timer: ReturnType<typeof setInterval>
onMounted(() => { refresh(); timer = setInterval(refresh, 5000) })
onUnmounted(() => clearInterval(timer))
</script>
