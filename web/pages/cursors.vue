<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Cursors &amp; Lag</h1>
        <p class="page-subtitle">How far behind each reader is — push, devices, custom consumers</p>
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
          @click="scan"
        >
          Refresh
        </v-btn>
      </div>
    </header>

    <div v-if="disabled" class="modern-card pa-12 text-center">
      <v-icon size="48" color="grey-lighten-1" class="mb-4">mdi-cursor-default-outline</v-icon>
      <h3 class="text-h6 font-weight-bold mb-2">Stream Log Disabled</h3>
      <code class="config-badge">messaging.stream.enabled: false</code>
    </div>

    <template v-else>
      <!-- Push dispatcher health: the alert that matters most -->
      <v-alert
        v-if="worstLag > 1000"
        type="error"
        variant="tonal"
        rounded="lg"
        class="mb-4"
        icon="mdi-timer-sand"
      >
        <div class="font-weight-bold">
          Push dispatcher is {{ formatCount(worstLag) }} records behind
        </div>
        <div class="text-body-2">
          Users are not being notified. The push webhook is failing or too slow to
          keep up with the log.
        </div>
      </v-alert>
      <v-alert
        v-else-if="worstLag > 100"
        type="warning"
        variant="tonal"
        rounded="lg"
        class="mb-4"
        icon="mdi-timer-sand"
      >
        Push dispatcher is {{ formatCount(worstLag) }} records behind on its slowest inbox.
      </v-alert>

      <v-row class="mb-2">
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Inboxes Tracked</div>
            <div class="metric-value" style="font-size: 1.5rem">{{ rows.length }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Worst Lag</div>
            <div class="metric-value" style="font-size: 1.5rem" :class="`text-${lagSeverity(worstLag)}`">
              {{ formatCount(worstLag) }}
            </div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Caught Up</div>
            <div class="metric-value" style="font-size: 1.5rem">
              {{ caughtUp }} / {{ rows.length }}
            </div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Cursors Stored</div>
            <div class="metric-value" style="font-size: 1.5rem">{{ formatCount(cursorsTracked) }}</div>
          </div>
        </v-col>
      </v-row>

      <!-- Group selector -->
      <div class="modern-card-flat pa-4 mb-4">
        <div class="d-flex flex-wrap align-center ga-3">
          <v-text-field
            v-model="group"
            density="compact"
            variant="outlined"
            hide-details
            rounded="lg"
            label="Cursor group"
            style="max-width: 260px"
            @keyup.enter="scan"
          />
          <v-text-field
            v-model="search"
            density="compact"
            variant="outlined"
            hide-details
            rounded="lg"
            placeholder="Filter topics"
            prepend-inner-icon="mdi-magnify"
            style="max-width: 300px"
            clearable
          />
          <v-spacer />
          <span class="text-caption text-medium-emphasis">
            <code>push-dispatcher</code> for notifications, <code>user:&lt;id&gt;</code> for a person
          </span>
        </div>
      </div>

      <div class="modern-card-flat">
        <v-table class="premium-table">
          <thead>
            <tr>
              <th>Topic</th>
              <th class="text-right">Part.</th>
              <th class="text-right">Watermark</th>
              <th class="text-right">Head</th>
              <th class="text-right">Lag</th>
              <th style="min-width: 160px">Progress</th>
              <th class="text-right">Members</th>
              <th />
            </tr>
          </thead>
          <tbody>
            <tr v-if="loading && !rows.length">
              <td colspan="8" class="text-center py-8">
                <v-progress-circular indeterminate color="primary" size="28" />
              </td>
            </tr>
            <tr v-else-if="!filtered.length">
              <td colspan="8" class="text-center text-medium-emphasis py-8">
                No cursors for group <code>{{ group }}</code>
              </td>
            </tr>
            <tr v-for="r in filtered" :key="`${r.topic}/${r.partition}`">
              <td>
                <div class="mono text-body-2">{{ topicSubject(r.topic) }}</div>
                <div class="text-caption text-medium-emphasis mono">{{ r.topic }}</div>
              </td>
              <td class="text-right mono">{{ r.partition }}</td>
              <td class="text-right mono">{{ r.watermark ?? '—' }}</td>
              <td class="text-right mono">{{ r.next_seq ?? '—' }}</td>
              <td class="text-right">
                <v-chip
                  :color="lagSeverity(r.lag)"
                  variant="tonal"
                  size="x-small"
                  class="mono font-weight-bold"
                >
                  {{ r.lag ?? 0 }}
                </v-chip>
              </td>
              <td>
                <v-progress-linear
                  :model-value="progress(r)"
                  :color="lagSeverity(r.lag)"
                  height="6"
                  rounded
                />
              </td>
              <td class="text-right mono">{{ Object.keys(r.members || {}).length }}</td>
              <td class="text-right">
                <v-btn variant="text" size="x-small" icon="mdi-chevron-right" @click="open(r)" />
              </td>
            </tr>
          </tbody>
        </v-table>
      </div>

      <p class="text-caption text-medium-emphasis mt-3">
        Lag is <code>next_seq − watermark</code>, where the watermark is the slowest
        member of the group. For a user group that is their least caught-up device;
        for <code>push-dispatcher</code> it is how far notifications trail the log.
      </p>
    </template>

    <!-- Per-member drill-down -->
    <v-dialog v-model="dialog" max-width="620">
      <div class="modern-card pa-6">
        <div class="d-flex align-center justify-space-between mb-1">
          <h3 class="text-h6 font-weight-bold">Members</h3>
          <v-btn variant="text" size="small" icon="mdi-close" @click="dialog = false" />
        </div>
        <p class="text-caption text-medium-emphasis mono mb-4">
          {{ detail?.topic }} · partition {{ detail?.partition }} · group {{ detail?.group }}
        </p>

        <div v-if="!detail || !Object.keys(detail.members || {}).length"
             class="text-body-2 text-medium-emphasis py-4">
          No members hold a cursor here.
        </div>

        <v-table v-else class="premium-table">
          <thead>
            <tr>
              <th>Member</th>
              <th class="text-right">Position</th>
              <th class="text-right">Behind</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(seq, member) in detail.members" :key="member">
              <td class="mono">{{ member || '(dispatcher)' }}</td>
              <td class="text-right mono">{{ seq }}</td>
              <td class="text-right">
                <v-chip
                  :color="lagSeverity((detail.next_seq ?? 0) - seq)"
                  variant="tonal"
                  size="x-small"
                  class="mono"
                >
                  {{ Math.max(0, (detail.next_seq ?? 0) - seq) }}
                </v-chip>
              </td>
            </tr>
          </tbody>
        </v-table>
      </div>
    </v-dialog>

    <v-snackbar v-model="snackbar" color="error" timeout="4000">{{ snackMessage }}</v-snackbar>
  </div>
</template>

<script setup lang="ts">
import type { CursorInfo } from '~/composables/useMessagingApi'

const api = useMessagingApi()
const loading = ref(false)
const disabled = ref(false)
const group = ref('push-dispatcher')
const search = ref('')
const rows = ref<CursorInfo[]>([])
const cursorsTracked = ref(0)
const autoRefresh = ref(true)
const snackbar = ref(false)
const snackMessage = ref('')

const dialog = ref(false)
const detail = ref<CursorInfo | null>(null)

const filtered = computed(() => {
  const q = (search.value || '').toLowerCase()
  return rows.value
    .filter((r) => !q || r.topic.toLowerCase().includes(q))
    .slice()
    .sort((a, b) => (b.lag ?? 0) - (a.lag ?? 0))
})

const worstLag = computed(() =>
  rows.value.reduce((max, r) => Math.max(max, r.lag ?? 0), 0),
)
const caughtUp = computed(() => rows.value.filter((r) => (r.lag ?? 0) === 0).length)

function progress(r: CursorInfo): number {
  const head = r.next_seq ?? 0
  const first = r.first_seq ?? 0
  const span = head - first
  if (span <= 0) return 100
  const done = (r.watermark ?? first) - first
  return Math.max(0, Math.min(100, (done / span) * 100))
}

function open(r: CursorInfo) {
  detail.value = r
  dialog.value = true
}

/**
 * There is no bulk cursor endpoint, so the page enumerates topics and asks per
 * partition. That is one request per partition — fine for an operator view at
 * human refresh rates, and the reason auto-refresh is 20s rather than 1s.
 */
async function scan() {
  loading.value = true
  try {
    const streams = await api.getStreams()
    disabled.value = false
    cursorsTracked.value = streams.cursors?.tracked ?? 0

    // For the push dispatcher only inbox topics are meaningful; for a user
    // group, conversations matter too.
    const relevant = streams.topics.filter((t) =>
      group.value === 'push-dispatcher' ? topicKind(t.name) === 'inbox' : true,
    )

    const results = await Promise.all(
      relevant.flatMap((t) =>
        t.partitions.map((p) =>
          api
            .getCursors(t.name, p.id, group.value)
            .catch(() => null),
        ),
      ),
    )

    rows.value = results.filter((r): r is CursorInfo => r !== null)
  } catch (e: any) {
    if (e?.statusCode === 404 || e?.response?.status === 404) {
      disabled.value = true
    } else {
      snackMessage.value = e?.data?.error || 'Failed to load cursors'
      snackbar.value = true
    }
  } finally {
    loading.value = false
  }
}

let timer: ReturnType<typeof setInterval> | null = null
function startTimer() {
  stopTimer()
  timer = setInterval(scan, 20000)
}
function stopTimer() {
  if (timer) clearInterval(timer)
  timer = null
}
watch(autoRefresh, (on) => (on ? startTimer() : stopTimer()))

onMounted(() => {
  scan()
  if (autoRefresh.value) startTimer()
})
onUnmounted(stopTimer)
</script>
