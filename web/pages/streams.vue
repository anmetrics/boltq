<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Streams</h1>
        <p class="page-subtitle">Partitioned message log — conversations, inboxes, history</p>
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

    <div v-if="disabled" class="modern-card pa-12 text-center">
      <v-icon size="48" color="grey-lighten-1" class="mb-4">mdi-database-off-outline</v-icon>
      <h3 class="text-h6 font-weight-bold mb-2">Stream Log Disabled</h3>
      <p class="text-body-2 text-medium-emphasis mb-4">
        No partitioned log is running.
      </p>
      <code class="config-badge">messaging.stream.enabled: false</code>
    </div>

    <template v-else>
      <!-- Summary -->
      <v-row class="mb-2">
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Topics</div>
            <div class="metric-value" style="font-size: 1.5rem">{{ topics.length }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Partitions</div>
            <div class="metric-value" style="font-size: 1.5rem">{{ totalPartitions }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Records</div>
            <div class="metric-value" style="font-size: 1.5rem">{{ formatCount(totalRecords) }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">On Disk</div>
            <div class="metric-value" style="font-size: 1.5rem">{{ formatBytes(totalBytes) }}</div>
            <div class="text-caption text-medium-emphasis">
              {{ formatCount(cursorsTracked) }} cursors
            </div>
          </div>
        </v-col>
      </v-row>

      <!-- Filters -->
      <div class="modern-card-flat pa-4 mb-4">
        <div class="d-flex flex-wrap align-center ga-3">
          <v-text-field
            v-model="search"
            density="compact"
            variant="outlined"
            hide-details
            rounded="lg"
            placeholder="Filter by topic or conversation ID"
            prepend-inner-icon="mdi-magnify"
            style="max-width: 340px"
            clearable
          />
          <v-btn-toggle v-model="kindFilter" density="compact" rounded="lg" variant="outlined" mandatory>
            <v-btn value="all" size="small">All</v-btn>
            <v-btn value="direct" size="small">Direct</v-btn>
            <v-btn value="group" size="small">Group</v-btn>
            <v-btn value="inbox" size="small">Inbox</v-btn>
            <v-btn value="other" size="small">Other</v-btn>
          </v-btn-toggle>
          <v-spacer />
          <v-chip
            v-if="skewedTopics.length"
            color="warning"
            variant="tonal"
            size="small"
            prepend-icon="mdi-scale-unbalanced"
          >
            {{ skewedTopics.length }} skewed
          </v-chip>
        </div>
      </div>

      <!-- Topic table -->
      <div class="modern-card-flat">
        <v-table class="premium-table">
          <thead>
            <tr>
              <th>Topic</th>
              <th>Kind</th>
              <th class="text-right">Partitions</th>
              <th class="text-right">Records</th>
              <th class="text-right">Size</th>
              <th class="text-right">Skew</th>
              <th class="text-right">Retention</th>
              <th />
            </tr>
          </thead>
          <tbody>
            <tr v-if="!filtered.length">
              <td colspan="8" class="text-center text-medium-emphasis py-8">
                {{ topics.length ? 'No topics match the filter' : 'No stream topics yet' }}
              </td>
            </tr>
            <tr v-for="t in filtered" :key="t.name">
              <td>
                <div class="mono text-body-2">{{ topicSubject(t.name) }}</div>
                <div class="text-caption text-medium-emphasis mono">{{ t.name }}</div>
              </td>
              <td>
                <v-chip :color="kindColor(t.name)" variant="tonal" size="x-small" class="font-weight-bold">
                  {{ topicKind(t.name) }}
                </v-chip>
              </td>
              <td class="text-right mono">{{ t.partitions.length }}</td>
              <td class="text-right mono">{{ formatCount(records(t)) }}</td>
              <td class="text-right mono">{{ formatBytes(t.total_bytes) }}</td>
              <td class="text-right">
                <v-chip
                  :color="skew(t) > 3 ? 'warning' : 'default'"
                  variant="tonal"
                  size="x-small"
                  class="mono"
                >
                  {{ skew(t).toFixed(1) }}×
                </v-chip>
              </td>
              <td class="text-right">
                <v-icon v-if="trimmed(t)" size="16" color="warning" title="Retention has removed history">
                  mdi-content-cut
                </v-icon>
                <span v-else class="text-medium-emphasis text-caption">full</span>
              </td>
              <td class="text-right">
                <v-btn
                  variant="text"
                  size="x-small"
                  icon="mdi-chevron-right"
                  @click="open(t.name)"
                />
              </td>
            </tr>
          </tbody>
        </v-table>
      </div>
    </template>

    <!-- Partition drill-down -->
    <v-dialog v-model="dialog" max-width="900">
      <div class="modern-card pa-6">
        <div class="d-flex align-center justify-space-between mb-1">
          <h3 class="text-h6 font-weight-bold">{{ topicSubject(detailName) }}</h3>
          <v-btn variant="text" size="small" icon="mdi-close" @click="dialog = false" />
        </div>
        <p class="text-caption text-medium-emphasis mono mb-4">{{ detailName }}</p>

        <div v-if="detailLoading" class="text-center py-8">
          <v-progress-circular indeterminate color="primary" />
        </div>

        <template v-else-if="detail">
          <v-table class="premium-table">
            <thead>
              <tr>
                <th class="text-right">Partition</th>
                <th class="text-right">First Seq</th>
                <th class="text-right">Next Seq</th>
                <th class="text-right">Records</th>
                <th class="text-right">Size</th>
                <th>Share</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="p in detail.partitions" :key="p.id">
                <td class="text-right mono">{{ p.id }}</td>
                <td class="text-right mono">{{ p.first_seq }}</td>
                <td class="text-right mono">{{ p.next_seq }}</td>
                <td class="text-right mono">{{ formatCount(p.records) }}</td>
                <td class="text-right mono">{{ formatBytes(p.bytes) }}</td>
                <td style="min-width: 140px">
                  <v-progress-linear
                    :model-value="share(p.bytes)"
                    :color="share(p.bytes) > 50 ? 'warning' : 'primary'"
                    height="6"
                    rounded
                  />
                </td>
              </tr>
            </tbody>
          </v-table>

          <v-alert
            v-if="detail.partitions.some((p) => p.first_seq > 1)"
            type="info"
            variant="tonal"
            density="compact"
            rounded="lg"
            class="mt-4"
          >
            Retention has removed history from at least one partition. Clients whose
            cursor falls below <code>first_seq</code> receive a <code>gap</code> frame
            and must resynchronise.
          </v-alert>
        </template>
      </div>
    </v-dialog>

    <v-snackbar v-model="snackbar" color="error" timeout="4000">{{ snackMessage }}</v-snackbar>
  </div>
</template>

<script setup lang="ts">
import type { StreamStats, TopicStats, PartitionStats } from '~/composables/useMessagingApi'

const api = useMessagingApi()
const loading = ref(false)
const disabled = ref(false)
const data = ref<StreamStats | null>(null)
const search = ref('')
const kindFilter = ref<'all' | 'direct' | 'group' | 'inbox' | 'other'>('all')
const snackbar = ref(false)
const snackMessage = ref('')

const dialog = ref(false)
const detailName = ref('')
const detail = ref<TopicStats | null>(null)
const detailLoading = ref(false)

const topics = computed(() => data.value?.topics ?? [])
const cursorsTracked = computed(() => data.value?.cursors?.tracked ?? 0)

const totalPartitions = computed(() =>
  topics.value.reduce((n, t) => n + t.partitions.length, 0),
)
const totalRecords = computed(() => topics.value.reduce((n, t) => n + records(t), 0))
const totalBytes = computed(() => topics.value.reduce((n, t) => n + t.total_bytes, 0))

function records(t: TopicStats): number {
  return t.partitions.reduce((n, p) => n + p.records, 0)
}

/**
 * Skew is the ratio of the largest partition to the mean. A high value means
 * one conversation is dominating, or the partition count is too low — either
 * way one lock is doing most of the work.
 */
function skew(t: TopicStats): number {
  if (t.partitions.length < 2) return 1
  const sizes = t.partitions.map((p) => p.records)
  const max = Math.max(...sizes)
  const mean = sizes.reduce((a, b) => a + b, 0) / sizes.length
  if (mean === 0) return 1
  return max / mean
}

function trimmed(t: TopicStats): boolean {
  return t.partitions.some((p) => p.first_seq > 1)
}

const skewedTopics = computed(() => topics.value.filter((t) => skew(t) > 3))

const filtered = computed(() => {
  const q = (search.value || '').toLowerCase()
  return topics.value
    .filter((t) => kindFilter.value === 'all' || topicKind(t.name) === kindFilter.value)
    .filter((t) => !q || t.name.toLowerCase().includes(q))
    .slice()
    .sort((a, b) => b.total_bytes - a.total_bytes)
})

function kindColor(name: string): string {
  switch (topicKind(name)) {
    case 'direct':
      return 'primary'
    case 'group':
      return 'success'
    case 'inbox':
      return 'info'
    default:
      return 'grey'
  }
}

function share(bytes: number): number {
  const total = detail.value?.total_bytes ?? 0
  if (!total) return 0
  return (bytes / total) * 100
}

async function open(name: string) {
  detailName.value = name
  detail.value = null
  dialog.value = true
  detailLoading.value = true
  try {
    detail.value = await api.getTopic(name)
  } catch (e: any) {
    snackMessage.value = e?.data?.error || `Failed to load ${name}`
    snackbar.value = true
    dialog.value = false
  } finally {
    detailLoading.value = false
  }
}

async function refresh() {
  loading.value = true
  try {
    data.value = await api.getStreams()
    disabled.value = false
  } catch (e: any) {
    if (e?.statusCode === 404 || e?.response?.status === 404) {
      disabled.value = true
    } else {
      snackMessage.value = e?.data?.error || 'Failed to load streams'
      snackbar.value = true
    }
  } finally {
    loading.value = false
  }
}

onMounted(refresh)
</script>
