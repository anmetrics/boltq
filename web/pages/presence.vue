<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Presence</h1>
        <p class="page-subtitle">Which devices are connected, and where</p>
      </div>
      <div class="d-flex align-center ga-2">
        <v-switch
          v-model="autoRefresh"
          color="primary"
          density="compact"
          hide-details
          label="Auto"
          class="mr-2"
        />
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

    <div v-if="disabled" class="modern-card pa-12 text-center">
      <v-icon size="48" color="grey-lighten-1" class="mb-4">mdi-account-off-outline</v-icon>
      <h3 class="text-h6 font-weight-bold mb-2">Presence Registry Unavailable</h3>
      <p class="text-body-2 text-medium-emphasis mb-4">
        Presence is part of the messaging subsystem.
      </p>
      <code class="config-badge">messaging.stream.enabled: false</code>
    </div>

    <template v-else>
      <v-row class="mb-2">
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Online Users</div>
            <div class="metric-value" style="font-size: 1.75rem">{{ formatCount(data?.users || 0) }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Live Sessions</div>
            <div class="metric-value" style="font-size: 1.75rem">{{ formatCount(data?.sessions || 0) }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Devices / User</div>
            <div class="metric-value" style="font-size: 1.75rem">{{ devicesPerUser }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Presence Watchers</div>
            <div class="metric-value" style="font-size: 1.75rem">{{ formatCount(data?.watchers || 0) }}</div>
          </div>
        </v-col>
      </v-row>

      <!-- Devices-per-user drift is the cheapest signal that device IDs are unstable -->
      <v-alert
        v-if="devicesPerUserNum > 4"
        type="warning"
        variant="tonal"
        rounded="lg"
        class="mb-4"
        icon="mdi-cellphone-link-off"
      >
        <div class="font-weight-bold">{{ devicesPerUser }} devices per user</div>
        <div class="text-body-2">
          Unusually high. The common cause is device IDs that change on reinstall,
          which also orphans a cursor per install and inflates storage.
        </div>
      </v-alert>

      <v-row>
        <v-col cols="12" md="4">
          <div class="modern-card pa-5 h-100">
            <h3 class="text-subtitle-1 font-weight-bold mb-4">By State</h3>
            <div v-if="!stateRows.length" class="text-body-2 text-medium-emphasis">
              No sessions.
            </div>
            <div v-for="r in stateRows" :key="r.key" class="mb-3">
              <div class="d-flex justify-space-between mb-1">
                <span class="text-body-2 d-flex align-center ga-2">
                  <v-icon size="14" :color="stateColor(r.key)">{{ stateIcon(r.key) }}</v-icon>
                  <span class="text-capitalize">{{ r.key }}</span>
                </span>
                <strong class="mono text-body-2">{{ formatCount(r.value) }}</strong>
              </div>
              <v-progress-linear
                :model-value="pct(r.value, data?.sessions || 0)"
                :color="stateColor(r.key)"
                height="6"
                rounded
              />
            </div>
            <p class="text-caption text-medium-emphasis mt-4 mb-0">
              <strong>away</strong> means connected but backgrounded — the state that
              still warrants a push notification.
            </p>
          </div>
        </v-col>

        <v-col cols="12" md="4">
          <div class="modern-card pa-5 h-100">
            <h3 class="text-subtitle-1 font-weight-bold mb-4">By Node</h3>
            <div v-if="!nodeRows.length" class="text-body-2 text-medium-emphasis">
              No sessions.
            </div>
            <div v-for="r in nodeRows" :key="r.key" class="mb-3">
              <div class="d-flex justify-space-between mb-1">
                <span class="text-body-2 mono">{{ r.key }}</span>
                <strong class="mono text-body-2">{{ formatCount(r.value) }}</strong>
              </div>
              <v-progress-linear
                :model-value="pct(r.value, data?.sessions || 0)"
                color="primary"
                height="6"
                rounded
              />
            </div>
            <p class="text-caption text-medium-emphasis mt-4 mb-0">
              This registry is per node — it does not gossip. In a sharded deployment
              each node reports only its own connections.
            </p>
          </div>
        </v-col>

        <v-col cols="12" md="4">
          <div class="modern-card pa-5 h-100">
            <h3 class="text-subtitle-1 font-weight-bold mb-4">By Region</h3>
            <div v-if="!regionRows.length" class="text-body-2 text-medium-emphasis">
              No region configured.
              <div class="mt-2">
                <code class="config-badge">messaging.presence.region</code>
              </div>
            </div>
            <div v-for="r in regionRows" :key="r.key" class="mb-3">
              <div class="d-flex justify-space-between mb-1">
                <span class="text-body-2 mono">{{ r.key }}</span>
                <strong class="mono text-body-2">{{ formatCount(r.value) }}</strong>
              </div>
              <v-progress-linear
                :model-value="pct(r.value, data?.sessions || 0)"
                color="info"
                height="6"
                rounded
              />
            </div>
          </div>
        </v-col>
      </v-row>
    </template>

    <v-snackbar v-model="snackbar" color="error" timeout="4000">{{ snackMessage }}</v-snackbar>
  </div>
</template>

<script setup lang="ts">
import type { PresenceStats } from '~/composables/useMessagingApi'

const api = useMessagingApi()
const loading = ref(false)
const disabled = ref(false)
const data = ref<PresenceStats | null>(null)
const autoRefresh = ref(true)
const snackbar = ref(false)
const snackMessage = ref('')

interface Row { key: string; value: number }

function toRows(m?: Record<string, number>): Row[] {
  if (!m) return []
  return Object.entries(m)
    .map(([key, value]) => ({ key, value }))
    .sort((a, b) => b.value - a.value)
}

const stateRows = computed(() => toRows(data.value?.by_state))
const nodeRows = computed(() => toRows(data.value?.by_node))
const regionRows = computed(() => toRows(data.value?.by_region))

const devicesPerUserNum = computed(() => {
  const d = data.value
  if (!d || !d.users) return 0
  return d.sessions / d.users
})
const devicesPerUser = computed(() =>
  devicesPerUserNum.value ? devicesPerUserNum.value.toFixed(2) : '—',
)

function pct(v: number, total: number): number {
  if (!total) return 0
  return (v / total) * 100
}

function stateColor(state: string): string {
  if (state === 'online') return 'success'
  if (state === 'away') return 'warning'
  return 'grey'
}

function stateIcon(state: string): string {
  if (state === 'online') return 'mdi-circle'
  if (state === 'away') return 'mdi-circle-slice-4'
  return 'mdi-circle-outline'
}

async function refresh() {
  loading.value = true
  try {
    data.value = await api.getPresence()
    disabled.value = false
  } catch (e: any) {
    if (e?.statusCode === 404 || e?.response?.status === 404) {
      disabled.value = true
    } else {
      snackMessage.value = e?.data?.error || 'Failed to load presence'
      snackbar.value = true
    }
  } finally {
    loading.value = false
  }
}

let timer: ReturnType<typeof setInterval> | null = null
function startTimer() {
  stopTimer()
  // Stats walks every shard, so keep the cadence gentle.
  timer = setInterval(refresh, 10000)
}
function stopTimer() {
  if (timer) clearInterval(timer)
  timer = null
}
watch(autoRefresh, (on) => (on ? startTimer() : stopTimer()))

onMounted(() => {
  refresh()
  if (autoRefresh.value) startTimer()
})
onUnmounted(stopTimer)
</script>
