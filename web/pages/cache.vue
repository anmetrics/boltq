<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Cache</h1>
        <p class="page-subtitle">In-memory KV store — browse, set, and manage cached data</p>
      </div>
      <div class="d-flex align-center ga-3">
        <v-btn
          variant="outlined"
          color="error"
          rounded="lg"
          size="small"
          prepend-icon="mdi-delete-sweep-outline"
          @click="flushDialog = true"
          :disabled="!cacheEnabled"
        >
          Flush All
        </v-btn>
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

    <!-- Cache disabled state -->
    <div v-if="cacheEnabled === false" class="modern-card pa-12 text-center">
      <v-icon size="48" color="grey-lighten-1" class="mb-4">mdi-database-off-outline</v-icon>
      <h3 class="text-h6 font-weight-bold mb-2">Cache Disabled</h3>
      <p class="text-body-2 text-medium-emphasis">
        Enable the KV store in the server configuration.
      </p>
      <code class="config-badge mt-4">cache.enabled: true</code>
    </div>

    <template v-else>
      <!-- Stats Cards -->
      <div class="stats-grid mb-6">
        <div v-for="card in statCards" :key="card.label" class="modern-card stat-card">
          <div class="stat-card-icon" :style="{ background: card.bgColor }">
            <v-icon :color="card.color" size="18">{{ card.icon }}</v-icon>
          </div>
          <div>
            <div class="metric-label">{{ card.label }}</div>
            <div class="stat-value">{{ card.value }}</div>
          </div>
        </div>
      </div>

      <!-- Add / Search bar -->
      <div class="modern-card pa-4 mb-6">
        <div class="d-flex align-center ga-3 flex-wrap">
          <v-text-field
            v-model="searchQuery"
            placeholder="Search keys..."
            prepend-inner-icon="mdi-magnify"
            density="compact"
            variant="outlined"
            hide-details
            class="search-field"
            style="max-width: 320px"
            @update:model-value="debouncedSearch"
          />
          <v-spacer />
          <v-btn
            color="primary"
            variant="flat"
            rounded="lg"
            size="small"
            prepend-icon="mdi-plus"
            @click="openSetDialog()"
          >
            Add Key
          </v-btn>
        </div>
      </div>

      <!-- Entries Table -->
      <div class="modern-card-flat">
        <v-table class="premium-table" density="compact">
          <thead>
            <tr>
              <th>Key</th>
              <th>Value</th>
              <th class="text-right">Size</th>
              <th class="text-right">TTL</th>
              <th class="text-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="entries.length === 0">
              <td colspan="5" class="text-center pa-10">
                <v-icon size="40" color="grey-lighten-1" class="mb-3">mdi-database-outline</v-icon>
                <div class="text-body-2 text-medium-emphasis">No entries found</div>
                <div class="text-caption text-medium-emphasis mt-1">
                  {{ searchQuery ? 'Try a different search term' : 'Add a key to get started' }}
                </div>
              </td>
            </tr>
            <tr v-for="entry in entries" :key="entry.key">
              <td>
                <div class="d-flex align-center">
                  <v-icon size="14" color="primary" class="mr-2" style="opacity: 0.4">mdi-key-variant</v-icon>
                  <span class="mono font-weight-medium entry-key">{{ entry.key }}</span>
                </div>
              </td>
              <td>
                <span class="mono entry-value" :title="formatValue(entry.value)">
                  {{ truncateValue(entry.value) }}
                </span>
              </td>
              <td class="text-right">
                <span class="text-caption text-medium-emphasis">{{ formatBytes(entry.size) }}</span>
              </td>
              <td class="text-right">
                <v-chip
                  v-if="entry.ttl === -1"
                  size="x-small"
                  variant="tonal"
                  class="font-weight-bold"
                >
                  No Expiry
                </v-chip>
                <v-chip
                  v-else-if="entry.ttl > 0"
                  size="x-small"
                  color="warning"
                  variant="tonal"
                  class="font-weight-bold"
                >
                  {{ formatTTL(entry.ttl) }}
                </v-chip>
                <v-chip
                  v-else
                  size="x-small"
                  color="error"
                  variant="tonal"
                  class="font-weight-bold"
                >
                  Expired
                </v-chip>
              </td>
              <td class="text-right">
                <div class="d-flex align-center justify-end ga-1">
                  <v-btn
                    icon="mdi-pencil-outline"
                    size="x-small"
                    variant="text"
                    color="primary"
                    @click="openSetDialog(entry)"
                  />
                  <v-btn
                    icon="mdi-delete-outline"
                    size="x-small"
                    variant="text"
                    color="error"
                    @click="deleteKey(entry.key)"
                  />
                </div>
              </td>
            </tr>
          </tbody>
        </v-table>
        <div class="table-footer" v-if="totalEntries > 0">
          <span class="text-caption text-medium-emphasis font-weight-medium">
            Showing {{ entries.length }} of {{ totalEntries }} entries
          </span>
        </div>
      </div>
    </template>

    <!-- Set Key Dialog -->
    <v-dialog v-model="setDialog" max-width="520">
      <v-card rounded="xl">
        <v-card-title class="text-body-1 font-weight-bold pt-5">
          {{ editingKey ? 'Edit Key' : 'Add Key' }}
        </v-card-title>
        <v-card-text>
          <v-text-field
            v-model="setForm.key"
            label="Key"
            placeholder="my:cache:key"
            class="mb-3"
            :disabled="!!editingKey"
          />
          <v-textarea
            v-model="setForm.value"
            label="Value"
            placeholder="Enter value (string or JSON)"
            rows="4"
            variant="outlined"
            class="mb-3 mono"
            style="font-size: 0.85rem"
          />
          <v-text-field
            v-model.number="setForm.ttl"
            label="TTL (milliseconds)"
            placeholder="0 = no expiry"
            type="number"
            hint="Leave 0 for no expiry"
          />
        </v-card-text>
        <v-card-actions class="pa-4 pt-0">
          <v-spacer />
          <v-btn variant="text" @click="setDialog = false">Cancel</v-btn>
          <v-btn
            color="primary"
            variant="flat"
            rounded="lg"
            :loading="saving"
            :disabled="!setForm.key"
            @click="saveKey"
          >
            {{ editingKey ? 'Update' : 'Create' }}
          </v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>

    <!-- Flush Confirmation -->
    <v-dialog v-model="flushDialog" max-width="420">
      <v-card rounded="xl">
        <v-card-title class="text-body-1 font-weight-bold pt-5">Flush All Cache</v-card-title>
        <v-card-text class="text-body-2">
          This will remove <strong>all</strong> cached entries. This action cannot be undone.
        </v-card-text>
        <v-card-actions class="pa-4 pt-0">
          <v-spacer />
          <v-btn variant="text" @click="flushDialog = false">Cancel</v-btn>
          <v-btn color="error" variant="flat" rounded="lg" :loading="flushing" @click="flushAll">
            Flush All
          </v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>

    <v-snackbar v-model="snackbar" :color="snackColor" timeout="3000" rounded="lg">
      {{ snackMessage }}
    </v-snackbar>
  </div>
</template>

<script setup lang="ts">
const api = useApi()
const { isDark } = useAppTheme()

const loading = ref(false)
const cacheEnabled = ref<boolean | null>(null)
const stats = ref<any>(null)
const entries = ref<any[]>([])
const totalEntries = ref(0)
const searchQuery = ref('')

// Dialogs
const setDialog = ref(false)
const editingKey = ref('')
const setForm = ref({ key: '', value: '', ttl: 0 })
const saving = ref(false)
const flushDialog = ref(false)
const flushing = ref(false)
const snackbar = ref(false)
const snackMessage = ref('')
const snackColor = ref('success')

const statCards = computed(() => {
  const s = stats.value || {}
  const d = isDark.value
  return [
    { label: 'Keys', value: formatNumber(s.key_count || 0), icon: 'mdi-key-variant', color: d ? '#818cf8' : '#6366f1', bgColor: d ? 'rgba(129,140,248,0.12)' : '#eef2ff' },
    { label: 'Memory', value: formatBytes(s.memory_used || 0), icon: 'mdi-memory', color: d ? '#22d3ee' : '#06b6d4', bgColor: d ? 'rgba(34,211,238,0.12)' : '#ecfeff' },
    { label: 'Hits', value: formatNumber(s.hits || 0), icon: 'mdi-check-circle-outline', color: d ? '#34d399' : '#10b981', bgColor: d ? 'rgba(52,211,153,0.12)' : '#ecfdf5' },
    { label: 'Misses', value: formatNumber(s.misses || 0), icon: 'mdi-close-circle-outline', color: d ? '#f87171' : '#ef4444', bgColor: d ? 'rgba(248,113,113,0.12)' : '#fef2f2' },
    { label: 'Hit Rate', value: hitRate(s), icon: 'mdi-percent-outline', color: d ? '#fbbf24' : '#f59e0b', bgColor: d ? 'rgba(251,191,36,0.12)' : '#fffbeb' },
    { label: 'Expired', value: formatNumber(s.expired || 0), icon: 'mdi-clock-alert-outline', color: d ? '#a78bfa' : '#8b5cf6', bgColor: d ? 'rgba(167,139,250,0.12)' : '#f5f3ff' },
  ]
})

function hitRate(s: any): string {
  const total = (s?.hits || 0) + (s?.misses || 0)
  if (total === 0) return '—'
  return ((s.hits / total) * 100).toFixed(1) + '%'
}

function formatNumber(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M'
  if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K'
  return n.toLocaleString()
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return '0 B'
  const k = 1024
  const sizes = ['B', 'KB', 'MB', 'GB']
  const i = Math.floor(Math.log(bytes) / Math.log(k))
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i]
}

function formatTTL(ms: number): string {
  if (ms < 1000) return ms + 'ms'
  if (ms < 60000) return (ms / 1000).toFixed(0) + 's'
  if (ms < 3600000) return (ms / 60000).toFixed(0) + 'm'
  return (ms / 3600000).toFixed(1) + 'h'
}

function formatValue(v: any): string {
  if (typeof v === 'object') return JSON.stringify(v)
  return String(v)
}

function truncateValue(v: any): string {
  const s = formatValue(v)
  return s.length > 80 ? s.substring(0, 80) + '...' : s
}

let searchTimeout: ReturnType<typeof setTimeout>
function debouncedSearch() {
  clearTimeout(searchTimeout)
  searchTimeout = setTimeout(() => loadEntries(), 300)
}

function openSetDialog(entry?: any) {
  if (entry) {
    editingKey.value = entry.key
    setForm.value = {
      key: entry.key,
      value: formatValue(entry.value),
      ttl: entry.ttl > 0 ? entry.ttl : 0,
    }
  } else {
    editingKey.value = ''
    setForm.value = { key: '', value: '', ttl: 0 }
  }
  setDialog.value = true
}

async function saveKey() {
  saving.value = true
  try {
    let value: any = setForm.value.value
    // Try to parse as JSON
    try { value = JSON.parse(value) } catch { /* keep as string */ }

    await api.cacheSet(setForm.value.key, value, setForm.value.ttl)
    snackMessage.value = `Key "${setForm.value.key}" saved`
    snackColor.value = 'success'
    snackbar.value = true
    setDialog.value = false
    await refresh()
  } catch (e: any) {
    snackMessage.value = e.data?.error || 'Failed to save'
    snackColor.value = 'error'
    snackbar.value = true
  } finally {
    saving.value = false
  }
}

async function deleteKey(key: string) {
  try {
    await api.cacheDel(key)
    snackMessage.value = `Key "${key}" deleted`
    snackColor.value = 'success'
    snackbar.value = true
    await refresh()
  } catch (e: any) {
    snackMessage.value = e.data?.error || 'Failed to delete'
    snackColor.value = 'error'
    snackbar.value = true
  }
}

async function flushAll() {
  flushing.value = true
  try {
    const result = await api.cacheFlush()
    snackMessage.value = `Flushed ${result.removed} entries`
    snackColor.value = 'success'
    snackbar.value = true
    flushDialog.value = false
    await refresh()
  } catch (e: any) {
    snackMessage.value = e.data?.error || 'Flush failed'
    snackColor.value = 'error'
    snackbar.value = true
  } finally {
    flushing.value = false
  }
}

async function loadEntries() {
  try {
    const data = await api.getCacheEntries('*', searchQuery.value)
    entries.value = data.entries || []
    totalEntries.value = data.total || 0
  } catch {}
}

async function loadStats() {
  try {
    const data = await api.getCacheStats()
    cacheEnabled.value = data.enabled !== false
    if (data.stats) stats.value = data.stats
  } catch {
    cacheEnabled.value = false
  }
}

async function refresh() {
  loading.value = true
  await Promise.all([loadStats(), loadEntries()])
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

.stats-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(160px, 1fr));
  gap: 12px;
}

.stat-card {
  display: flex;
  align-items: center;
  gap: 14px;
  padding: 16px 18px;
}

.stat-card-icon {
  width: 36px;
  height: 36px;
  border-radius: 10px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.stat-value {
  font-size: 1.15rem;
  font-weight: 700;
  color: var(--text-primary);
  letter-spacing: -0.01em;
}

.entry-key {
  color: var(--primary);
  font-size: 0.85rem;
}

.entry-value {
  font-size: 0.8rem;
  color: var(--text-muted);
  max-width: 300px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  display: inline-block;
}

.table-footer {
  padding: 12px 16px;
  border-top: 1px solid var(--border-color);
}

.config-badge {
  display: inline-block;
  padding: 6px 16px;
  background: var(--bg-tertiary);
  border: 1px solid var(--border-color);
  border-radius: 8px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 0.8rem;
  color: var(--text-muted);
}

.search-field {
  :deep(.v-field) {
    border-radius: 10px;
  }
}

.ga-1 { gap: 4px; }
.ga-3 { gap: 12px; }
</style>
