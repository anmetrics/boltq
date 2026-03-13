<template>
  <div>
    <div class="d-flex align-center mb-6">
      <div>
        <h1 class="text-h4 font-weight-bold">Metrics</h1>
        <p class="text-body-2 mt-1" style="opacity: 0.5">Real-time server metrics</p>
      </div>
      <v-spacer />
      <v-chip size="small" :color="autoRefresh ? 'primary' : 'default'" variant="flat" class="mr-2" @click="autoRefresh = !autoRefresh">
        <v-icon size="14" class="mr-1">mdi-autorenew</v-icon>
        Auto-refresh {{ autoRefresh ? 'ON' : 'OFF' }}
      </v-chip>
      <v-btn icon="mdi-refresh" variant="text" size="small" :loading="loading" @click="refresh" />
    </div>

    <v-row>
      <v-col v-for="m in metricItems" :key="m.key" cols="12" sm="6" md="4" lg="3">
        <v-card class="metric-card pa-4" color="surface">
          <div class="d-flex align-center mb-3">
            <v-icon :color="m.color" size="20" class="mr-2">{{ m.icon }}</v-icon>
            <span class="metric-label" style="opacity: 0.7">{{ m.label }}</span>
          </div>
          <div class="metric-value" :style="{ color: m.color, fontSize: '1.8rem' }">
            {{ formatNumber(metrics[m.key] || 0) }}
          </div>
          <div v-if="m.description" class="text-caption mt-2" style="opacity: 0.35">
            {{ m.description }}
          </div>
        </v-card>
      </v-col>
    </v-row>

    <!-- Prometheus endpoint info -->
    <v-card class="data-table-card mt-6 pa-4" color="surface">
      <div class="d-flex align-center mb-2">
        <v-icon size="18" class="mr-2" style="opacity: 0.5">mdi-information-outline</v-icon>
        <span class="text-body-2 font-weight-medium">Prometheus Integration</span>
      </div>
      <p class="text-body-2 mb-2" style="opacity: 0.5">
        Scrape metrics in Prometheus format:
      </p>
      <v-code class="mono pa-2" style="background: rgba(0,0,0,0.3); border-radius: 8px; display: block">
        curl http://localhost:9090/metrics
      </v-code>
    </v-card>
  </div>
</template>

<script setup lang="ts">
const api = useApi()
const loading = ref(false)
const metrics = ref<Record<string, number>>({})
const autoRefresh = ref(true)

const metricItems = [
  { key: 'messages_published', label: 'Published', icon: 'mdi-upload', color: '#4fc3f7', description: 'Total messages published to queues' },
  { key: 'messages_consumed', label: 'Consumed', icon: 'mdi-download', color: '#00d4aa', description: 'Total messages consumed from queues' },
  { key: 'messages_acked', label: 'Acknowledged', icon: 'mdi-check-circle', color: '#66bb6a', description: 'Successfully acknowledged messages' },
  { key: 'messages_nacked', label: 'Nacked', icon: 'mdi-close-circle', color: '#ef5350', description: 'Negatively acknowledged (retried)' },
  { key: 'retry_count', label: 'Retries', icon: 'mdi-refresh', color: '#ffa726', description: 'Total retry attempts' },
  { key: 'dead_letter_count', label: 'Dead Letters', icon: 'mdi-email-alert', color: '#e57373', description: 'Messages sent to dead letter queue' },
  { key: 'raft_apply_count', label: 'Raft Applies', icon: 'mdi-database-sync', color: '#7c4dff', description: 'Raft log entries applied (cluster)' },
  { key: 'snapshot_count', label: 'Snapshots', icon: 'mdi-camera', color: '#4db6ac', description: 'Raft snapshots taken (cluster)' },
  { key: 'leader_changes', label: 'Leader Changes', icon: 'mdi-swap-horizontal', color: '#ff8a65', description: 'Raft leader elections (cluster)' },
]

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
