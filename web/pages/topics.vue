<template>
  <div class="premium-dashboard">
    <header class="dashboard-header d-flex align-center mb-10">
      <div>
        <h1 class="text-h4 font-weight-black gradient-text-primary mb-1">Global Topics</h1>
        <div class="text-caption text-muted font-weight-bold letter-spacing-1">REAL-TIME BROADCAST CHANNELS</div>
      </div>
      <v-spacer />
      <v-btn
        variant="tonal"
        color="secondary"
        rounded="lg"
        prepend-icon="mdi-refresh"
        :loading="loading"
        @click="refresh"
      >
        REFRESH
      </v-btn>
    </header>

    <div class="glass-card table-glass pa-0">

      <v-table class="premium-table">
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
      <div class="footer-stats d-flex align-center px-6 py-3" style="border-top: 1px solid var(--glass-border)">
        <v-icon size="14" class="mr-2 text-muted">mdi-information-outline</v-icon>
        <span class="text-caption text-muted font-weight-bold">DISCOVERED TOPICS: {{ rows.length }}</span>
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
