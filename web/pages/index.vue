<template>
  <div>
    <!-- Header -->
    <header class="page-header">
      <div>
        <h1 class="page-title">Dashboard</h1>
        <p class="page-subtitle">Monitor your message queue infrastructure in real-time</p>
      </div>
      <div class="d-flex align-center ga-3">
        <div class="live-badge" v-if="isOnline">
          <span class="live-dot" />
          <span>Live</span>
        </div>
        <v-btn
          variant="outlined"
          color="primary"
          rounded="lg"
          size="small"
          :loading="loading"
          @click="refresh"
          prepend-icon="mdi-refresh"
        >
          Refresh
        </v-btn>
      </div>
    </header>

    <!-- Metrics Grid -->
    <div class="metrics-grid">
      <div
        v-for="card in metricCards"
        :key="card.label"
        class="modern-card metric-card"
      >
        <div class="metric-card-header">
          <div class="metric-icon" :style="{ background: card.bgColor }">
            <v-icon :color="card.color" size="20">{{ card.icon }}</v-icon>
          </div>
          <span class="metric-label">{{ card.label }}</span>
        </div>
        <div class="metric-value">{{ formatNumber(card.value) }}</div>
      </div>
    </div>

    <!-- Charts & System -->
    <v-row class="mt-6">
      <v-col cols="12" lg="8">
        <div class="modern-card pa-6">
          <div class="d-flex align-center justify-space-between mb-6">
            <h3 class="card-title">Throughput</h3>
            <div class="d-flex align-center ga-5">
              <div class="legend-item">
                <span class="legend-dot" style="background: var(--primary)" />
                <span>Published</span>
              </div>
              <div class="legend-item">
                <span class="legend-dot" style="background: var(--success)" />
                <span>Consumed</span>
              </div>
            </div>
          </div>
          <div class="chart-wrapper">
            <v-chart class="chart" :option="throughputOption" autoresize />
          </div>
        </div>
      </v-col>

      <v-col cols="12" lg="4">
        <div class="d-flex flex-column ga-4 h-100">
          <!-- System -->
          <div class="modern-card pa-5">
            <h3 class="card-title mb-4">System</h3>
            <div class="system-item mb-4">
              <div class="d-flex align-center justify-space-between mb-2">
                <span class="system-label">Goroutines</span>
                <span class="system-value">{{ overview?.system?.goroutines || 0 }}</span>
              </div>
              <v-progress-linear :model-value="goroutinePercent" color="primary" height="6" rounded />
            </div>
            <div class="system-item mb-4">
              <div class="d-flex align-center justify-space-between mb-2">
                <span class="system-label">Heap Memory</span>
                <span class="system-value">{{ formatBytes(overview?.system?.memory || 0) }}</span>
              </div>
              <v-progress-linear :model-value="memoryPercent" color="secondary" height="6" rounded />
            </div>
            <div class="system-item">
              <div class="d-flex align-center justify-space-between mb-2">
                <span class="system-label">Uptime</span>
                <span class="system-value mono">{{ overview?.uptime_ms ? formatUptime(overview.uptime_ms) : '--:--:--' }}</span>
              </div>
            </div>
          </div>

          <!-- Storage -->
          <div class="modern-card pa-5 flex-grow-1">
            <h3 class="card-title mb-4">Storage</h3>
            <div class="storage-highlight">
              {{ formatBytes(overview?.storage?.size || 0) }}
            </div>
            <v-progress-linear
              :model-value="storagePercent"
              color="accent"
              height="6"
              rounded
              class="mb-3 mt-4"
            />
            <div class="d-flex justify-space-between">
              <span class="text-caption text-medium-emphasis">
                Mode: {{ overview?.storage?.mode?.toUpperCase() || 'N/A' }}
              </span>
              <span class="text-caption text-medium-emphasis">
                Threshold: {{ formatBytes(overview?.storage?.compaction_threshold || 0) }}
              </span>
            </div>
          </div>
        </div>
      </v-col>
    </v-row>

    <!-- Tables -->
    <v-row class="mt-6">
      <v-col cols="12" md="6">
        <div class="modern-card-flat">
          <div class="card-table-header">
            <div class="d-flex align-center">
              <v-icon color="primary" size="20" class="mr-2">mdi-tray-full</v-icon>
              <h3 class="card-title">Queues</h3>
            </div>
            <v-btn to="/queues" variant="text" color="primary" size="small" rounded="lg">
              View all
            </v-btn>
          </div>
          <v-table class="premium-table" density="compact">
            <thead>
              <tr>
                <th>Name</th>
                <th class="text-right">Messages</th>
                <th class="text-right">Dead Letters</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="q in queueRows.slice(0, 5)" :key="q.name">
                <td class="mono font-weight-medium" style="color: var(--primary)">{{ q.name }}</td>
                <td class="text-right mono font-weight-medium">{{ formatNumber(q.messages) }}</td>
                <td class="text-right">
                  <v-chip v-if="q.deadLetters > 0" size="x-small" color="error" variant="tonal" class="font-weight-bold">
                    {{ q.deadLetters }}
                  </v-chip>
                  <span v-else class="text-medium-emphasis">-</span>
                </td>
              </tr>
              <tr v-if="queueRows.length === 0">
                <td colspan="3" class="text-center text-medium-emphasis pa-6">No queues yet</td>
              </tr>
            </tbody>
          </v-table>
        </div>
      </v-col>

      <v-col cols="12" md="6">
        <div class="modern-card-flat">
          <div class="card-table-header">
            <div class="d-flex align-center">
              <v-icon color="success" size="20" class="mr-2">mdi-broadcast</v-icon>
              <h3 class="card-title">Topics</h3>
            </div>
            <v-btn to="/topics" variant="text" color="success" size="small" rounded="lg">
              View all
            </v-btn>
          </div>
          <v-table class="premium-table" density="compact">
            <thead>
              <tr>
                <th>Name</th>
                <th class="text-right">Subscribers</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="t in topicRows.slice(0, 5)" :key="t.name">
                <td class="mono font-weight-medium" style="color: var(--success)">{{ t.name }}</td>
                <td class="text-right">
                  <v-chip size="x-small" :color="t.subscribers > 0 ? 'success' : 'default'" variant="tonal" class="font-weight-bold">
                    {{ t.subscribers }} subs
                  </v-chip>
                </td>
              </tr>
              <tr v-if="topicRows.length === 0">
                <td colspan="2" class="text-center text-medium-emphasis pa-6">No topics yet</td>
              </tr>
            </tbody>
          </v-table>
        </div>
      </v-col>
    </v-row>
  </div>
</template>

<script setup lang="ts">
import { useIntervalFn } from '@vueuse/core'

const api = useApi()
const { isOnline, setOnline } = useServerStatus()

const loading = ref(false)
const overview = ref<any>(null)
const history = ref<{ time: string; published: number; consumed: number }[]>([])
const MAX_HISTORY = 30

const { isDark } = useAppTheme()

const metricCards = computed(() => {
  const m = overview.value?.metrics || {}
  const d = isDark.value
  return [
    { label: 'Published', value: m.messages_published || 0, icon: 'mdi-arrow-up-circle-outline', color: d ? '#818cf8' : '#6366f1', bgColor: d ? 'rgba(129,140,248,0.12)' : '#eef2ff' },
    { label: 'Consumed', value: m.messages_consumed || 0, icon: 'mdi-arrow-down-circle-outline', color: d ? '#34d399' : '#10b981', bgColor: d ? 'rgba(52,211,153,0.12)' : '#ecfdf5' },
    { label: 'Pending', value: overview.value?.stats?.PendingCount || 0, icon: 'mdi-clock-outline', color: d ? '#fbbf24' : '#f59e0b', bgColor: d ? 'rgba(251,191,36,0.12)' : '#fffbeb' },
    { label: 'Acknowledged', value: m.messages_acked || 0, icon: 'mdi-check-circle-outline', color: d ? '#34d399' : '#10b981', bgColor: d ? 'rgba(52,211,153,0.12)' : '#ecfdf5' },
    { label: 'Nacked', value: m.messages_nacked || 0, icon: 'mdi-close-circle-outline', color: d ? '#f87171' : '#ef4444', bgColor: d ? 'rgba(248,113,113,0.12)' : '#fef2f2' },
    { label: 'Dead Letters', value: m.dead_letter_count || 0, icon: 'mdi-alert-circle-outline', color: d ? '#f87171' : '#ef4444', bgColor: d ? 'rgba(248,113,113,0.12)' : '#fef2f2' },
  ]
})

const goroutinePercent = computed(() => {
  const g = overview.value?.system?.goroutines || 0
  return Math.min((g / 1000) * 100, 100)
})

const memoryPercent = computed(() => {
  const mem = overview.value?.system?.memory || 0
  const maxMem = 1024 * 1024 * 1024 // 1GB as reference
  return Math.min((mem / maxMem) * 100, 100)
})

const storagePercent = computed(() => {
  const size = overview.value?.storage?.size || 0
  const threshold = overview.value?.storage?.compaction_threshold || 104857600
  return Math.min((size / threshold) * 100, 100)
})

const queueRows = computed(() => {
  const queues = overview.value?.stats?.Queues || {}
  const dls = overview.value?.stats?.DeadLetters || {}
  return Object.entries(queues).map(([name, messages]) => ({
    name,
    messages: messages as number,
    deadLetters: (dls[name + '_dead_letter'] || 0) as number,
  }))
})

const topicRows = computed(() => {
  const topics = overview.value?.stats?.Topics || {}
  return Object.entries(topics).map(([name, subscribers]) => ({
    name,
    subscribers: subscribers as number,
  }))
})

const throughputOption = computed(() => {
  const d = isDark.value
  const primaryLine = d ? '#818cf8' : '#6366f1'
  const successLine = d ? '#34d399' : '#10b981'
  return {
  backgroundColor: 'transparent',
  grid: { left: '0%', right: '2%', bottom: '0%', top: '10%', containLabel: true },
  tooltip: {
    trigger: 'axis',
    backgroundColor: d ? '#1a1d27' : '#fff',
    borderColor: d ? '#2e3345' : '#e5e7eb',
    borderWidth: 1,
    padding: [10, 14],
    textStyle: { color: d ? '#e2e8f0' : '#374151', fontFamily: 'Inter', fontSize: 12 },
    axisPointer: { lineStyle: { color: d ? '#2e3345' : '#e5e7eb', type: 'dashed' } },
  },
  xAxis: {
    type: 'category',
    data: history.value.map(h => h.time),
    axisLine: { show: false },
    axisTick: { show: false },
    axisLabel: { color: d ? '#64748b' : '#9ca3af', fontSize: 11, margin: 12 },
  },
  yAxis: {
    type: 'value',
    splitLine: { lineStyle: { color: d ? '#242836' : '#f3f4f6', type: 'dashed' } },
    axisLabel: { color: d ? '#64748b' : '#9ca3af', fontSize: 11 },
  },
  series: [
    {
      name: 'Published',
      type: 'line',
      smooth: 0.4,
      showSymbol: false,
      data: history.value.map(h => h.published),
      lineStyle: { width: 2.5, color: primaryLine },
      areaStyle: {
        opacity: 0.08,
        color: { type: 'linear', x: 0, y: 0, x2: 0, y2: 1, colorStops: [{ offset: 0, color: primaryLine }, { offset: 1, color: 'transparent' }] },
      },
    },
    {
      name: 'Consumed',
      type: 'line',
      smooth: 0.4,
      showSymbol: false,
      data: history.value.map(h => h.consumed),
      lineStyle: { width: 2.5, color: successLine },
      areaStyle: {
        opacity: 0.08,
        color: { type: 'linear', x: 0, y: 0, x2: 0, y2: 1, colorStops: [{ offset: 0, color: successLine }, { offset: 1, color: 'transparent' }] },
      },
    },
  ],
}})

function formatNumber(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + 'M'
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K'
  return n.toLocaleString()
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i]
}

function formatUptime(ms: number): string {
  const seconds = Math.floor(ms / 1000)
  const h = Math.floor(seconds / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const s = seconds % 60
  return `${h.toString().padStart(2, '0')}:${m.toString().padStart(2, '0')}:${s.toString().padStart(2, '0')}`
}

async function refresh() {
  loading.value = true
  try {
    const data = await api.getOverview()
    const now = new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' })
    const lastMetrics = overview.value?.metrics

    if (lastMetrics) {
      const pubSec = Math.max(0, data.metrics.messages_published - lastMetrics.messages_published)
      const conSec = Math.max(0, data.metrics.messages_consumed - lastMetrics.messages_consumed)
      history.value.push({ time: now, published: pubSec, consumed: conSec })
      if (history.value.length > MAX_HISTORY) history.value.shift()
    }

    overview.value = data
    setOnline(true)
  } catch {
    setOnline(false)
  } finally {
    loading.value = false
  }
}

useIntervalFn(refresh, 3000)
onMounted(refresh)
</script>

<style lang="scss" scoped>
.page-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  margin-bottom: 28px;
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

.live-badge {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 4px 12px;
  background: var(--success-light);
  border: 1px solid rgba(16, 185, 129, 0.2);
  border-radius: 20px;
  font-size: 0.75rem;
  font-weight: 600;
  color: var(--success);
}

.live-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: var(--success);
  animation: pulse 2s infinite;
}

@keyframes pulse {
  0% { opacity: 1; }
  50% { opacity: 0.4; }
  100% { opacity: 1; }
}

.metrics-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
  gap: 16px;
}

.metric-card {
  padding: 20px;

  .metric-card-header {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 14px;
  }

  .metric-icon {
    width: 36px;
    height: 36px;
    border-radius: 10px;
    display: flex;
    align-items: center;
    justify-content: center;
  }

  .metric-value {
    font-size: 1.75rem;
  }
}

.card-title {
  font-size: 0.95rem;
  font-weight: 600;
  color: var(--text-primary);
}

.chart-wrapper {
  height: 320px;
}

.legend-item {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 0.75rem;
  color: var(--text-muted);
  font-weight: 500;
}

.legend-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
}

.system-label {
  font-size: 0.8rem;
  color: var(--text-muted);
  font-weight: 500;
}

.system-value {
  font-size: 0.85rem;
  font-weight: 600;
  color: var(--text-primary);
}

.storage-highlight {
  font-size: 1.75rem;
  font-weight: 700;
  color: var(--text-primary);
  letter-spacing: -0.02em;
}

.card-table-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 16px 16px 12px;
  border-bottom: 1px solid var(--border-color);
}

.ga-3 { gap: 12px; }
.ga-4 { gap: 16px; }
.ga-5 { gap: 20px; }
</style>
