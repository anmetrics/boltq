<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Topics</h1>
        <p class="page-subtitle">Manage broadcast channels and subscribers</p>
      </div>
      <v-btn
        variant="outlined"
        color="primary"
        rounded="lg"
        size="small"
        prepend-icon="mdi-refresh"
        :loading="loading"
        @click="refresh"
      >
        Refresh
      </v-btn>
    </header>

    <div class="modern-card-flat">
      <v-table class="premium-table">
        <thead>
          <tr>
            <th>Topic Name</th>
            <th class="text-right">Subscribers</th>
            <th class="text-right">Status</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="rows.length === 0">
            <td colspan="3" class="text-center pa-10">
              <v-icon size="40" color="grey-lighten-1" class="mb-3">mdi-broadcast-off</v-icon>
              <div class="text-body-2 text-medium-emphasis">No topics established</div>
              <div class="text-caption text-medium-emphasis mt-1">Subscribe via TCP to initialize a topic</div>
            </td>
          </tr>
          <tr v-for="t in rows" :key="t.name">
            <td>
              <div class="d-flex align-center">
                <v-icon size="16" color="success" class="mr-2" style="opacity: 0.5">mdi-broadcast</v-icon>
                <span class="mono font-weight-medium">{{ t.name }}</span>
              </div>
            </td>
            <td class="text-right">
              <v-chip
                :color="t.subscribers > 0 ? 'success' : 'default'"
                size="small"
                variant="tonal"
                class="font-weight-bold"
              >
                {{ t.subscribers }} subs
              </v-chip>
            </td>
            <td class="text-right">
              <div class="d-flex align-center justify-end ga-2">
                <span class="status-dot" :class="t.subscribers > 0 ? 'online' : 'offline'" style="width: 6px; height: 6px" />
                <span class="text-caption font-weight-medium" :class="t.subscribers > 0 ? 'text-success' : 'text-medium-emphasis'">
                  {{ t.subscribers > 0 ? 'Active' : 'Idle' }}
                </span>
              </div>
            </td>
          </tr>
        </tbody>
      </v-table>
      <div class="table-footer">
        <div class="d-flex align-center">
          <v-icon size="14" class="mr-2 text-medium-emphasis">mdi-information-outline</v-icon>
          <span class="text-caption text-medium-emphasis font-weight-medium">
            Total: {{ rows.length }} topics
          </span>
        </div>
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

<style lang="scss" scoped>
.page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  margin-bottom: 24px;
}
.page-title {
  font-size: 1.5rem;
  font-weight: 700;
  color: var(--text-primary);
  letter-spacing: -0.02em;
}
.page-subtitle {
  font-size: 0.875rem;
  color: var(--text-muted);
  margin-top: 2px;
}
.table-footer {
  padding: 12px 16px;
  border-top: 1px solid var(--border-color);
}
.ga-2 { gap: 8px; }
</style>
