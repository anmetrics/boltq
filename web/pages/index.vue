<template>
  <div>
    <div class="d-flex align-center mb-6">
      <div>
        <h1 class="text-h4 font-weight-bold">Dashboard</h1>
        <p class="text-body-2 mt-1" style="opacity: 0.5">BoltQ Message Queue Overview</p>
      </div>
      <v-spacer />
      <v-chip :color="serverOnline ? 'success' : 'error'" variant="flat" size="small" class="mr-2">
        <span class="status-dot mr-2" :class="serverOnline ? 'online' : 'offline'" />
        {{ serverOnline ? 'Online' : 'Offline' }}
      </v-chip>
      <v-btn icon="mdi-refresh" variant="text" size="small" :loading="loading" @click="refresh" />
    </div>

    <!-- Metric cards -->
    <v-row class="mb-4">
      <v-col v-for="card in metricCards" :key="card.label" cols="12" sm="6" md="4" lg="2">
        <v-card class="metric-card pa-4" color="surface">
          <div class="d-flex align-center mb-2">
            <v-icon :color="card.color" size="20">{{ card.icon }}</v-icon>
          </div>
          <div class="metric-value" :style="{ color: card.color }">
            {{ formatNumber(card.value) }}
          </div>
          <div class="metric-label mt-1">{{ card.label }}</div>
        </v-card>
      </v-col>
    </v-row>

    <!-- Queue overview + Cluster status -->
    <v-row>
      <v-col cols="12" md="8">
        <v-card class="data-table-card" color="surface">
          <v-card-title class="text-body-1 font-weight-bold pa-4 pb-2">
            <v-icon size="18" class="mr-2">mdi-tray-full</v-icon>
            Work Queues
          </v-card-title>
          <v-table density="compact" hover>
            <thead>
              <tr>
                <th>Queue</th>
                <th class="text-right">Messages</th>
                <th class="text-right">Dead Letters</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="queueRows.length === 0">
                <td colspan="3" class="text-center pa-6" style="opacity: 0.4">No queues yet</td>
              </tr>
              <tr v-for="q in queueRows" :key="q.name">
                <td class="mono">{{ q.name }}</td>
                <td class="text-right mono">{{ q.messages }}</td>
                <td class="text-right mono">
                  <v-chip v-if="q.deadLetters > 0" color="error" size="x-small" variant="flat">
                    {{ q.deadLetters }}
                  </v-chip>
                  <span v-else style="opacity: 0.3">0</span>
                </td>
              </tr>
            </tbody>
          </v-table>
          <div class="pa-3 text-caption" style="opacity: 0.3">
            Pending ACKs: {{ overview?.stats?.PendingCount || 0 }}
          </div>
        </v-card>
      </v-col>

      <v-col cols="12" md="4">
        <!-- Cluster card -->
        <v-card class="data-table-card mb-4" color="surface">
          <v-card-title class="text-body-1 font-weight-bold pa-4 pb-2">
            <v-icon size="18" class="mr-2">mdi-server-network</v-icon>
            Cluster
          </v-card-title>
          <v-card-text v-if="!overview?.cluster?.enabled">
            <v-chip color="grey" size="small" variant="flat">Disabled</v-chip>
          </v-card-text>
          <v-card-text v-else>
            <div class="d-flex align-center mb-2">
              <v-chip :color="overview.cluster.cluster?.state === 'Leader' ? 'success' : 'info'" size="small" variant="flat">
                {{ overview.cluster.cluster?.state }}
              </v-chip>
            </div>
            <div class="text-caption mb-1" style="opacity: 0.5">Node: {{ overview.cluster.cluster?.node_id }}</div>
            <div class="text-caption mb-1" style="opacity: 0.5">Leader: {{ overview.cluster.cluster?.leader_id }}</div>
            <div class="text-caption" style="opacity: 0.5">
              Peers: {{ overview.cluster.cluster?.peers?.length || 0 }}
            </div>
          </v-card-text>
        </v-card>

        <!-- Topics card -->
        <v-card class="data-table-card" color="surface">
          <v-card-title class="text-body-1 font-weight-bold pa-4 pb-2">
            <v-icon size="18" class="mr-2">mdi-broadcast</v-icon>
            Pub/Sub Topics
          </v-card-title>
          <v-card-text v-if="topicRows.length === 0">
            <div style="opacity: 0.4">No topics yet</div>
          </v-card-text>
          <v-list v-else density="compact" bg-color="transparent">
            <v-list-item v-for="t in topicRows" :key="t.name">
              <template #title>
                <span class="mono text-body-2">{{ t.name }}</span>
              </template>
              <template #append>
                <v-chip size="x-small" color="secondary" variant="flat">
                  {{ t.subscribers }} sub{{ t.subscribers !== 1 ? 's' : '' }}
                </v-chip>
              </template>
            </v-list-item>
          </v-list>
        </v-card>
      </v-col>
    </v-row>
  </div>
</template>

<script setup lang="ts">
const api = useApi()

const loading = ref(false)
const serverOnline = ref(false)
const overview = ref<any>(null)

const metricCards = computed(() => {
  const m = overview.value?.metrics || {}
  return [
    { label: 'Published', value: m.messages_published || 0, icon: 'mdi-upload', color: '#4fc3f7' },
    { label: 'Consumed', value: m.messages_consumed || 0, icon: 'mdi-download', color: '#00d4aa' },
    { label: 'Pending', value: overview.value?.stats?.PendingCount || 0, icon: 'mdi-clock-outline', color: '#ffa726' },
    { label: 'Acked', value: m.messages_acked || 0, icon: 'mdi-check-circle', color: '#66bb6a' },
    { label: 'Nacked', value: m.messages_nacked || 0, icon: 'mdi-close-circle', color: '#ef5350' },
    { label: 'Dead Letters', value: m.dead_letter_count || 0, icon: 'mdi-email-alert', color: '#e57373' },
  ]
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

function formatNumber(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K'
  return n.toString()
}

async function refresh() {
  loading.value = true
  try {
    overview.value = await api.getOverview()
    serverOnline.value = true
  } catch {
    serverOnline.value = false
  } finally {
    loading.value = false
  }
}

let timer: ReturnType<typeof setInterval>

onMounted(() => {
  refresh()
  timer = setInterval(refresh, 5000)
})

onUnmounted(() => {
  clearInterval(timer)
})
</script>
