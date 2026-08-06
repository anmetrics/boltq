<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Messaging</h1>
        <p class="page-subtitle">Chat backbone — streams, presence, delivery</p>
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

    <!-- Subsystem disabled -->
    <div v-if="disabled" class="modern-card pa-12 text-center">
      <v-icon size="48" color="grey-lighten-1" class="mb-4">mdi-message-off-outline</v-icon>
      <h3 class="text-h6 font-weight-bold mb-2">Messaging Subsystem Disabled</h3>
      <p class="text-body-2 text-medium-emphasis mb-4">
        The partitioned log, gateway and presence registry are off. Enable them to
        serve chat traffic.
      </p>
      <code class="config-badge">messaging.stream.enabled: false</code>
    </div>

    <template v-else>
      <!-- Alerts: the things that mean something is wrong right now -->
      <v-alert
        v-if="pushDropped > 0"
        type="error"
        variant="tonal"
        rounded="lg"
        class="mb-4"
        icon="mdi-bell-off"
      >
        <div class="font-weight-bold">{{ formatCount(pushDropped) }} push notifications abandoned</div>
        <div class="text-body-2">
          Batches exceeded <code>max_attempts</code> and the cursor advanced past them.
          Those users were never told about their messages. Check the push webhook.
        </div>
      </v-alert>

      <v-alert
        v-if="slowDrops > 0"
        type="warning"
        variant="tonal"
        rounded="lg"
        class="mb-4"
        icon="mdi-transmission-tower-off"
      >
        <div class="font-weight-bold">{{ formatCount(slowDrops) }} slow-client disconnects</div>
        <div class="text-body-2">
          Clients were disconnected because their send queue filled. They resume from
          their cursor with nothing lost, but a rising rate means a client bug or a
          <code>send_buffer</code> that is too small.
        </div>
      </v-alert>

      <!-- Headline numbers -->
      <v-row class="mb-2">
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Connected Devices</div>
            <div class="metric-value" style="font-size: 1.75rem">
              {{ formatCount(presence?.sessions || 0) }}
            </div>
            <div class="text-caption text-medium-emphasis">
              {{ formatCount(presence?.users || 0) }} users
            </div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Stream Topics</div>
            <div class="metric-value" style="font-size: 1.75rem">
              {{ formatCount(topicCount) }}
            </div>
            <div class="text-caption text-medium-emphasis">
              {{ formatCount(partitionCount) }} partitions
            </div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Messages Stored</div>
            <div class="metric-value" style="font-size: 1.75rem">
              {{ formatCount(totalRecords) }}
            </div>
            <div class="text-caption text-medium-emphasis">
              {{ formatBytes(totalBytes) }} on disk
            </div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Push Delivered</div>
            <div class="metric-value" style="font-size: 1.75rem">
              {{ formatCount(push?.sent || 0) }}
            </div>
            <div class="text-caption text-medium-emphasis">
              {{ formatCount(push?.suppressed || 0) }} suppressed (online)
            </div>
          </div>
        </v-col>
      </v-row>

      <v-row>
        <!-- Gateway -->
        <v-col cols="12" md="6">
          <div class="modern-card pa-5 h-100">
            <div class="d-flex align-center justify-space-between mb-4">
              <h3 class="text-subtitle-1 font-weight-bold">Gateway</h3>
              <NuxtLink to="/gateway" class="text-caption text-decoration-none" style="color: var(--primary)">
                Details →
              </NuxtLink>
            </div>
            <div v-if="!gateway" class="text-body-2 text-medium-emphasis">
              Gateway not enabled.
            </div>
            <template v-else>
              <div class="stat-row">
                <span>Attached sessions</span>
                <strong>{{ formatCount(gateway.attached) }}</strong>
              </div>
              <div class="stat-row">
                <span>Tracked sessions</span>
                <strong>{{ formatCount(gateway.sessions) }}</strong>
              </div>
              <div class="stat-row">
                <span>Resume hit rate</span>
                <strong :class="resumeRateClass">{{ resumeRate }}</strong>
              </div>
              <div class="stat-row">
                <span>Auth failures</span>
                <strong>{{ formatCount(gateway.auth_failures) }}</strong>
              </div>
              <div class="stat-row">
                <span>ACL denials</span>
                <strong :class="gateway.forbidden > 0 ? 'text-warning' : ''">
                  {{ formatCount(gateway.forbidden) }}
                </strong>
              </div>
            </template>
          </div>
        </v-col>

        <!-- Push -->
        <v-col cols="12" md="6">
          <div class="modern-card pa-5 h-100">
            <div class="d-flex align-center justify-space-between mb-4">
              <h3 class="text-subtitle-1 font-weight-bold">Push Dispatch</h3>
              <NuxtLink to="/cursors" class="text-caption text-decoration-none" style="color: var(--primary)">
                Lag →
              </NuxtLink>
            </div>
            <div v-if="!push" class="text-body-2 text-medium-emphasis">
              Push dispatch not enabled.
            </div>
            <template v-else>
              <div class="stat-row">
                <span>Inboxes watched</span>
                <strong>{{ formatCount(push.watching) }}</strong>
              </div>
              <div class="stat-row">
                <span>Records scanned</span>
                <strong>{{ formatCount(push.scanned) }}</strong>
              </div>
              <div class="stat-row">
                <span>Sent</span>
                <strong>{{ formatCount(push.sent) }}</strong>
              </div>
              <div class="stat-row">
                <span>Suppressed (recipient online)</span>
                <strong>{{ formatCount(push.suppressed) }}</strong>
              </div>
              <div class="stat-row">
                <span>Failed attempts</span>
                <strong>{{ formatCount(push.failed) }}</strong>
              </div>
              <div class="stat-row">
                <span>Abandoned</span>
                <strong :class="push.dropped > 0 ? 'text-error' : ''">
                  {{ formatCount(push.dropped) }}
                </strong>
              </div>
            </template>
          </div>
        </v-col>

        <!-- Presence -->
        <v-col cols="12" md="6">
          <div class="modern-card pa-5 h-100">
            <div class="d-flex align-center justify-space-between mb-4">
              <h3 class="text-subtitle-1 font-weight-bold">Presence</h3>
              <NuxtLink to="/presence" class="text-caption text-decoration-none" style="color: var(--primary)">
                Details →
              </NuxtLink>
            </div>
            <div v-if="!presence" class="text-body-2 text-medium-emphasis">
              Presence not enabled.
            </div>
            <template v-else>
              <div class="stat-row">
                <span>Devices per user</span>
                <strong>{{ devicesPerUser }}</strong>
              </div>
              <div v-for="(n, state) in presence.by_state" :key="state" class="stat-row">
                <span class="text-capitalize">{{ state }}</span>
                <strong>{{ formatCount(n) }}</strong>
              </div>
              <div class="stat-row">
                <span>Presence watchers</span>
                <strong>{{ formatCount(presence.watchers) }}</strong>
              </div>
            </template>
          </div>
        </v-col>

        <!-- Ephemeral signals -->
        <v-col cols="12" md="6">
          <div class="modern-card pa-5 h-100">
            <h3 class="text-subtitle-1 font-weight-bold mb-4">Ephemeral Signals</h3>
            <div v-if="!signals" class="text-body-2 text-medium-emphasis">
              Signal hub not enabled.
            </div>
            <template v-else>
              <div class="stat-row">
                <span>Published</span>
                <strong>{{ formatCount(signals.published) }}</strong>
              </div>
              <div class="stat-row">
                <span>Delivered</span>
                <strong>{{ formatCount(signals.delivered) }}</strong>
              </div>
              <div class="stat-row">
                <span>Dropped (slow subscriber)</span>
                <strong>{{ formatCount(signals.dropped) }}</strong>
              </div>
              <div class="stat-row">
                <span>Rate limited</span>
                <strong>{{ formatCount(signals.rate_limited) }}</strong>
              </div>
              <div class="stat-row">
                <span>Live topics</span>
                <strong>{{ formatCount(signals.topics) }}</strong>
              </div>
              <p class="text-caption text-medium-emphasis mt-3 mb-0">
                Drops here are expected and harmless — typing indicators are best effort
                by design.
              </p>
            </template>
          </div>
        </v-col>
      </v-row>
    </template>

    <v-snackbar v-model="snackbar" :color="snackColor" timeout="4000">
      {{ snackMessage }}
    </v-snackbar>
  </div>
</template>

<script setup lang="ts">
import type { MessagingOverview } from '~/composables/useMessagingApi'

const api = useMessagingApi()
const loading = ref(false)
const disabled = ref(false)
const data = ref<MessagingOverview | null>(null)
const autoRefresh = ref(true)
const snackbar = ref(false)
const snackMessage = ref('')
const snackColor = ref('error')

const streams = computed(() => data.value?.streams ?? null)
const presence = computed(() => data.value?.presence ?? null)
const gateway = computed(() => data.value?.gateway ?? null)
const signals = computed(() => data.value?.signals ?? null)
const push = computed(() => data.value?.push ?? null)

const pushDropped = computed(() => push.value?.dropped ?? 0)
const slowDrops = computed(() => gateway.value?.slow_client_drops ?? 0)

const topicCount = computed(() => streams.value?.topics?.length ?? 0)
const partitionCount = computed(() =>
  (streams.value?.topics ?? []).reduce((n, t) => n + (t.partitions?.length ?? 0), 0),
)
const totalRecords = computed(() =>
  (streams.value?.topics ?? []).reduce(
    (n, t) => n + (t.partitions ?? []).reduce((m, p) => m + p.records, 0),
    0,
  ),
)
const totalBytes = computed(() =>
  (streams.value?.topics ?? []).reduce((n, t) => n + t.total_bytes, 0),
)

const devicesPerUser = computed(() => {
  const p = presence.value
  if (!p || !p.users) return '—'
  return (p.sessions / p.users).toFixed(2)
})

const resumeRate = computed(() => {
  const g = gateway.value
  if (!g || !g.connections) return '—'
  return `${((g.resumed / g.connections) * 100).toFixed(1)}%`
})

// A low resume rate means clients are not storing their resume token, so every
// reconnect pays for a full re-subscribe.
const resumeRateClass = computed(() => {
  const g = gateway.value
  if (!g || !g.connections) return ''
  return g.resumed / g.connections < 0.3 ? 'text-warning' : ''
})

let timer: ReturnType<typeof setInterval> | null = null

async function refresh() {
  loading.value = true
  try {
    data.value = await api.getOverview()
    disabled.value = false
  } catch (e: any) {
    // A 404 means the endpoints were never registered, i.e. the subsystem is off.
    if (e?.statusCode === 404 || e?.response?.status === 404) {
      disabled.value = true
    } else {
      snackMessage.value = e?.data?.error || 'Failed to load messaging stats'
      snackbar.value = true
    }
  } finally {
    loading.value = false
  }
}

watch(autoRefresh, (on) => {
  if (on) startTimer()
  else stopTimer()
})

function startTimer() {
  stopTimer()
  // The overview walks every topic and partition, so poll gently.
  timer = setInterval(refresh, 15000)
}
function stopTimer() {
  if (timer) clearInterval(timer)
  timer = null
}

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
.stat-row span {
  color: var(--text-secondary, rgba(128, 128, 128, 0.9));
}
</style>
