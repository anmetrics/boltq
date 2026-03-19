<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Metrics</h1>
        <p class="page-subtitle">System telemetry and performance counters</p>
      </div>
      <div class="d-flex align-center ga-3">
        <v-chip
          size="small"
          :color="autoRefresh ? 'success' : 'default'"
          variant="tonal"
          class="font-weight-bold cursor-pointer"
          @click="autoRefresh = !autoRefresh"
        >
          {{ autoRefresh ? 'Live' : 'Paused' }}
        </v-chip>
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
      </div>
    </header>

    <v-row>
      <v-col v-for="m in metricItems" :key="m.key" cols="12" sm="6" md="4" lg="3">
        <div class="modern-card pa-5">
          <div class="d-flex align-center justify-space-between mb-3">
            <span class="metric-label">{{ m.label }}</span>
            <div class="metric-icon-sm" :style="{ background: m.bgColor }">
              <v-icon :color="m.color" size="16">{{ m.icon }}</v-icon>
            </div>
          </div>
          <div class="metric-value" style="font-size: 1.75rem">
            {{ formatNumber(metrics[m.key] || 0) }}
          </div>
          <div v-if="m.description" class="text-caption text-medium-emphasis mt-3">
            {{ m.description }}
          </div>
        </div>
      </v-col>
    </v-row>

    <!-- Prometheus -->
    <div class="modern-card pa-5 mt-6">
      <div class="d-flex align-center mb-3">
        <v-icon size="18" class="mr-2 text-medium-emphasis">mdi-information-outline</v-icon>
        <span class="card-title">Prometheus Integration</span>
      </div>
      <p class="text-body-2 text-medium-emphasis mb-3">
        Scrape metrics in Prometheus format:
      </p>
      <code class="code-block">curl http://localhost:9090/metrics</code>
    </div>
  </div>
</template>

<script setup lang="ts">
const api = useApi()
const loading = ref(false)
const metrics = ref<Record<string, number>>({})
const autoRefresh = ref(true)

const { isDark } = useAppTheme()

const metricItems = computed(() => {
  const d = isDark.value
  return [
    { key: 'messages_published', label: 'Published', icon: 'mdi-arrow-up-circle-outline', color: d ? '#818cf8' : '#6366f1', bgColor: d ? 'rgba(129,140,248,0.12)' : '#eef2ff', description: 'Total messages published to queues' },
    { key: 'messages_consumed', label: 'Consumed', icon: 'mdi-arrow-down-circle-outline', color: d ? '#34d399' : '#10b981', bgColor: d ? 'rgba(52,211,153,0.12)' : '#ecfdf5', description: 'Total messages consumed from queues' },
    { key: 'messages_acked', label: 'Acknowledged', icon: 'mdi-check-circle-outline', color: d ? '#34d399' : '#10b981', bgColor: d ? 'rgba(52,211,153,0.12)' : '#ecfdf5', description: 'Successfully acknowledged messages' },
    { key: 'messages_nacked', label: 'Nacked', icon: 'mdi-close-circle-outline', color: d ? '#f87171' : '#ef4444', bgColor: d ? 'rgba(248,113,113,0.12)' : '#fef2f2', description: 'Negatively acknowledged (retried)' },
    { key: 'retry_count', label: 'Retries', icon: 'mdi-refresh', color: d ? '#fbbf24' : '#f59e0b', bgColor: d ? 'rgba(251,191,36,0.12)' : '#fffbeb', description: 'Total retry attempts' },
    { key: 'dead_letter_count', label: 'Dead Letters', icon: 'mdi-email-alert-outline', color: d ? '#f87171' : '#ef4444', bgColor: d ? 'rgba(248,113,113,0.12)' : '#fef2f2', description: 'Messages sent to dead letter queue' },
    { key: 'raft_apply_count', label: 'Raft Applies', icon: 'mdi-database-sync-outline', color: d ? '#a78bfa' : '#8b5cf6', bgColor: d ? 'rgba(167,139,250,0.12)' : '#f5f3ff', description: 'Raft log entries applied (cluster)' },
    { key: 'snapshot_count', label: 'Snapshots', icon: 'mdi-camera-outline', color: d ? '#22d3ee' : '#06b6d4', bgColor: d ? 'rgba(34,211,238,0.12)' : '#ecfeff', description: 'Raft snapshots taken (cluster)' },
    { key: 'leader_changes', label: 'Leader Changes', icon: 'mdi-swap-horizontal', color: d ? '#fb923c' : '#f97316', bgColor: d ? 'rgba(249,115,22,0.12)' : '#fff7ed', description: 'Raft leader elections (cluster)' },
  ]
})

function formatNumber(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K'
  return n.toLocaleString()
}

async function refresh() {
  loading.value = true
  try { metrics.value = await api.getMetrics() } catch {}
  loading.value = false
}

let timer: ReturnType<typeof setInterval>

function startTimer() {
  timer = setInterval(() => {
    if (autoRefresh.value) refresh()
  }, 3000)
}

onMounted(() => { refresh(); startTimer() })
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
.card-title {
  font-size: 0.95rem;
  font-weight: 600;
  color: var(--text-primary);
}
.metric-icon-sm {
  width: 32px;
  height: 32px;
  border-radius: 8px;
  display: flex;
  align-items: center;
  justify-content: center;
}
.code-block {
  display: block;
  padding: 12px 16px;
  background: var(--bg-tertiary);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 0.8rem;
  color: var(--text-secondary);
}
.cursor-pointer { cursor: pointer; }
.ga-3 { gap: 12px; }
</style>
