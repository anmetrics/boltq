<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Dead Letters</h1>
        <p class="page-subtitle">Messages that exceeded max retry attempts</p>
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

    <div class="modern-card-flat">
      <v-table class="premium-table">
        <thead>
          <tr>
            <th>Dead Letter Queue</th>
            <th>Source Queue</th>
            <th class="text-right">Messages</th>
            <th class="text-right">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="rows.length === 0">
            <td colspan="4" class="text-center pa-10">
              <v-icon size="40" color="success" class="mb-3" style="opacity: 0.4">mdi-check-circle</v-icon>
              <div class="text-body-2 text-medium-emphasis">No dead letters</div>
              <div class="text-caption text-medium-emphasis mt-1">All messages are being processed successfully</div>
            </td>
          </tr>
          <tr v-for="dl in rows" :key="dl.name">
            <td>
              <div class="d-flex align-center">
                <v-icon size="16" color="error" class="mr-2" style="opacity: 0.5">mdi-email-alert</v-icon>
                <span class="mono font-weight-medium">{{ dl.name }}</span>
              </div>
            </td>
            <td>
              <span class="mono text-medium-emphasis">{{ dl.sourceQueue }}</span>
            </td>
            <td class="text-right">
              <v-chip color="error" size="small" variant="tonal" class="font-weight-bold">
                {{ dl.count.toLocaleString() }}
              </v-chip>
            </td>
            <td class="text-right">
              <v-btn
                size="x-small"
                variant="tonal"
                color="error"
                rounded="lg"
                :disabled="dl.count === 0"
                @click="purge(dl.sourceQueue)"
              >
                Purge
              </v-btn>
            </td>
          </tr>
        </tbody>
      </v-table>
      <div class="table-footer">
        <span class="text-caption text-medium-emphasis font-weight-medium">
          Total dead letter messages: {{ totalDead }}
        </span>
      </div>
    </div>

    <v-dialog v-model="purgeDialog" max-width="420">
      <v-card rounded="xl">
        <v-card-title class="text-body-1 font-weight-bold pt-5">Purge Dead Letters</v-card-title>
        <v-card-text class="text-body-2">
          Are you sure you want to purge dead letters for
          <strong class="mono">{{ purgeTarget }}</strong>?
        </v-card-text>
        <v-card-actions class="pa-4 pt-0">
          <v-spacer />
          <v-btn variant="text" @click="purgeDialog = false">Cancel</v-btn>
          <v-btn color="error" variant="flat" rounded="lg" :loading="purging" @click="confirmPurge">Purge</v-btn>
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
const loading = ref(false)
const stats = ref<any>(null)
const purgeDialog = ref(false)
const purgeTarget = ref('')
const purging = ref(false)
const snackbar = ref(false)
const snackMessage = ref('')
const snackColor = ref('success')

const rows = computed(() => {
  const dls = stats.value?.DeadLetters || {}
  return Object.entries(dls).map(([name, count]) => ({
    name,
    sourceQueue: name.replace(/_dead_letter$/, ''),
    count: count as number,
  })).sort((a, b) => a.name.localeCompare(b.name))
})

const totalDead = computed(() => rows.value.reduce((sum, dl) => sum + dl.count, 0))

function purge(source: string) {
  purgeTarget.value = source
  purgeDialog.value = true
}

async function confirmPurge() {
  purging.value = true
  try {
    const result = await api.purgeDeadLetters(purgeTarget.value)
    snackMessage.value = `Purged ${result.purged_count} dead letters`
    snackColor.value = 'success'
    snackbar.value = true
    purgeDialog.value = false
    await refresh()
  } catch (e: any) {
    snackMessage.value = e.data?.error || 'Purge failed'
    snackColor.value = 'error'
    snackbar.value = true
  } finally {
    purging.value = false
  }
}

async function refresh() {
  loading.value = true
  try { stats.value = await api.getStats() } catch {}
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
.table-footer {
  padding: 12px 16px;
  border-top: 1px solid var(--border-color);
}
</style>
