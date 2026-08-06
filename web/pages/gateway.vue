<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Gateway</h1>
        <p class="page-subtitle">WebSocket edge — end-user connections and sessions</p>
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
      <v-icon size="48" color="grey-lighten-1" class="mb-4">mdi-lan-disconnect</v-icon>
      <h3 class="text-h6 font-weight-bold mb-2">Gateway Disabled</h3>
      <p class="text-body-2 text-medium-emphasis mb-4">
        No WebSocket edge is running, so end-user devices cannot connect.
      </p>
      <code class="config-badge">messaging.gateway.enabled: false</code>
    </div>

    <template v-else>
      <v-alert
        v-if="forbiddenRate > 0"
        type="warning"
        variant="tonal"
        rounded="lg"
        class="mb-4"
        icon="mdi-shield-alert"
      >
        <div class="font-weight-bold">{{ formatCount(data!.forbidden) }} authorisation denials</div>
        <div class="text-body-2">
          A correct client should never be denied. Sustained denials mean either a
          client bug or someone probing for conversations they do not belong to.
        </div>
      </v-alert>

      <v-row class="mb-2">
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Attached Now</div>
            <div class="metric-value" style="font-size: 1.75rem">
              {{ formatCount(data?.attached || 0) }}
            </div>
            <div class="text-caption text-medium-emphasis">live sockets</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Tracked Sessions</div>
            <div class="metric-value" style="font-size: 1.75rem">
              {{ formatCount(data?.sessions || 0) }}
            </div>
            <div class="text-caption text-medium-emphasis">
              {{ formatCount(detached) }} in resume window
            </div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Resume Hit Rate</div>
            <div class="metric-value" style="font-size: 1.75rem" :class="resumeClass">
              {{ resumeRate }}
            </div>
            <div class="text-caption text-medium-emphasis">
              {{ formatCount(data?.resumed || 0) }} resumed
            </div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Records Delivered</div>
            <div class="metric-value" style="font-size: 1.75rem">
              {{ formatCount(data?.records_out || 0) }}
            </div>
            <div class="text-caption text-medium-emphasis">since start</div>
          </div>
        </v-col>
      </v-row>

      <v-row>
        <v-col cols="12" md="6">
          <div class="modern-card pa-5 h-100">
            <h3 class="text-subtitle-1 font-weight-bold mb-4">Traffic</h3>
            <div class="stat-row">
              <span>Connections accepted</span>
              <strong class="mono">{{ formatCount(data?.connections || 0) }}</strong>
            </div>
            <div class="stat-row">
              <span>Frames in</span>
              <strong class="mono">{{ formatCount(data?.frames_in || 0) }}</strong>
            </div>
            <div class="stat-row">
              <span>Frames out</span>
              <strong class="mono">{{ formatCount(data?.frames_out || 0) }}</strong>
            </div>
            <div class="stat-row">
              <span>Records out</span>
              <strong class="mono">{{ formatCount(data?.records_out || 0) }}</strong>
            </div>
            <div class="stat-row">
              <span>Frames per connection</span>
              <strong class="mono">{{ framesPerConn }}</strong>
            </div>
          </div>
        </v-col>

        <v-col cols="12" md="6">
          <div class="modern-card pa-5 h-100">
            <h3 class="text-subtitle-1 font-weight-bold mb-4">Health</h3>
            <div class="stat-row">
              <span>
                Auth failures
                <v-tooltip text="Rejected before the WebSocket upgrade — expired tokens on reconnect are normal" location="top">
                  <template #activator="{ props }">
                    <v-icon v-bind="props" size="13" class="ml-1">mdi-help-circle-outline</v-icon>
                  </template>
                </v-tooltip>
              </span>
              <strong class="mono">{{ formatCount(data?.auth_failures || 0) }}</strong>
            </div>
            <div class="stat-row">
              <span>ACL denials</span>
              <strong class="mono" :class="forbiddenRate > 0 ? 'text-warning' : ''">
                {{ formatCount(data?.forbidden || 0) }}
              </strong>
            </div>
            <div class="stat-row">
              <span>
                Slow-client drops
                <v-tooltip text="Clients disconnected because their send queue filled. They resume from their cursor — nothing is lost" location="top">
                  <template #activator="{ props }">
                    <v-icon v-bind="props" size="13" class="ml-1">mdi-help-circle-outline</v-icon>
                  </template>
                </v-tooltip>
              </span>
              <strong class="mono" :class="(data?.slow_client_drops || 0) > 0 ? 'text-warning' : ''">
                {{ formatCount(data?.slow_client_drops || 0) }}
              </strong>
            </div>
            <div class="stat-row">
              <span>Detached (resumable)</span>
              <strong class="mono">{{ formatCount(detached) }}</strong>
            </div>
          </div>
        </v-col>
      </v-row>

      <div class="modern-card pa-5 mt-4">
        <h3 class="text-subtitle-1 font-weight-bold mb-3">Session lifecycle</h3>
        <div class="d-flex align-center ga-4 flex-wrap">
          <div class="lifecycle-box">
            <div class="text-h5 font-weight-bold" style="color: var(--primary)">
              {{ formatCount(data?.attached || 0) }}
            </div>
            <div class="text-caption text-medium-emphasis">Attached</div>
          </div>
          <v-icon color="grey">mdi-arrow-right</v-icon>
          <div class="lifecycle-box">
            <div class="text-h5 font-weight-bold text-warning">{{ formatCount(detached) }}</div>
            <div class="text-caption text-medium-emphasis">Detached</div>
          </div>
          <v-icon color="grey">mdi-arrow-right</v-icon>
          <div class="lifecycle-box">
            <div class="text-h5 font-weight-bold text-success">{{ formatCount(data?.resumed || 0) }}</div>
            <div class="text-caption text-medium-emphasis">Resumed</div>
          </div>
        </div>
        <p class="text-caption text-medium-emphasis mt-4 mb-0">
          A large detached count means clients are dropping and reconnecting often —
          network problems, or a <code>pong_timeout</code> that is too aggressive. A low
          resume rate means clients are not storing their resume token, so every
          reconnect pays for a full re-subscribe.
        </p>
      </div>
    </template>

    <v-snackbar v-model="snackbar" color="error" timeout="4000">{{ snackMessage }}</v-snackbar>
  </div>
</template>

<script setup lang="ts">
import type { GatewayStats } from '~/composables/useMessagingApi'

const api = useMessagingApi()
const loading = ref(false)
const disabled = ref(false)
const data = ref<GatewayStats | null>(null)
const autoRefresh = ref(true)
const snackbar = ref(false)
const snackMessage = ref('')

const detached = computed(() => {
  const d = data.value
  if (!d) return 0
  return Math.max(0, d.sessions - d.attached)
})

const resumeRate = computed(() => {
  const d = data.value
  if (!d || !d.connections) return '—'
  return `${((d.resumed / d.connections) * 100).toFixed(1)}%`
})

const resumeClass = computed(() => {
  const d = data.value
  if (!d || !d.connections) return ''
  return d.resumed / d.connections < 0.3 ? 'text-warning' : ''
})

const framesPerConn = computed(() => {
  const d = data.value
  if (!d || !d.connections) return '—'
  return ((d.frames_in + d.frames_out) / d.connections).toFixed(1)
})

const forbiddenRate = computed(() => data.value?.forbidden ?? 0)

async function refresh() {
  loading.value = true
  try {
    data.value = await api.getGatewayStats()
    disabled.value = data.value === null
  } catch (e: any) {
    if (e?.statusCode === 404 || e?.response?.status === 404) {
      disabled.value = true
    } else {
      snackMessage.value = e?.data?.error || 'Failed to load gateway stats'
      snackbar.value = true
    }
  } finally {
    loading.value = false
  }
}

let timer: ReturnType<typeof setInterval> | null = null
function startTimer() {
  stopTimer()
  timer = setInterval(refresh, 5000)
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

<style scoped>
.stat-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0.5rem 0;
  font-size: 0.875rem;
  border-bottom: 1px solid rgba(128, 128, 128, 0.12);
}
.stat-row:last-child {
  border-bottom: none;
}
.stat-row > span {
  color: var(--text-secondary, rgba(128, 128, 128, 0.9));
  display: flex;
  align-items: center;
}
.lifecycle-box {
  min-width: 110px;
  text-align: center;
  padding: 0.75rem 1rem;
  border-radius: 12px;
  background: rgba(128, 128, 128, 0.06);
}
</style>
