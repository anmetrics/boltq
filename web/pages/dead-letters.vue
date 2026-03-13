<template>
  <div>
    <div class="d-flex align-center mb-6">
      <div>
        <h1 class="text-h4 font-weight-bold">Dead Letter Queues</h1>
        <p class="text-body-2 mt-1" style="opacity: 0.5">Messages that exceeded max retry attempts</p>
      </div>
      <v-spacer />
      <v-btn icon="mdi-refresh" variant="text" size="small" :loading="loading" @click="refresh" />
    </div>

    <v-card class="data-table-card" color="surface">
      <v-table hover>
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
            <td colspan="4" class="text-center pa-8" style="opacity: 0.4">
              <v-icon size="48" class="mb-2" color="success" style="opacity: 0.5">mdi-check-circle</v-icon>
              <div>No dead letters</div>
              <div class="text-caption mt-1">All messages are being processed successfully</div>
            </td>
          </tr>
          <tr v-for="dl in rows" :key="dl.name">
            <td>
              <v-icon size="16" color="error" class="mr-1">mdi-email-alert</v-icon>
              <span class="mono font-weight-medium">{{ dl.name }}</span>
            </td>
            <td>
              <span class="mono" style="opacity: 0.5">{{ dl.sourceQueue }}</span>
            </td>
            <td class="text-right">
              <v-chip color="error" size="small" variant="flat">
                {{ dl.count.toLocaleString() }}
              </v-chip>
            </td>
            <td class="text-right">
              <v-btn
                size="x-small"
                variant="tonal"
                color="error"
                :disabled="dl.count === 0"
                @click="purge(dl.sourceQueue)"
              >
                Purge
              </v-btn>
            </td>
          </tr>
        </tbody>
      </v-table>
      <div class="pa-4 text-caption" style="opacity: 0.3; border-top: 1px solid rgba(255,255,255,0.06)">
        Total dead letter messages: {{ totalDead }}
      </div>
    </v-card>

    <v-dialog v-model="purgeDialog" max-width="400">
      <v-card color="surface">
        <v-card-title>Purge Dead Letters</v-card-title>
        <v-card-text>
          Are you sure you want to purge dead letters for
          <strong class="mono">{{ purgeTarget }}</strong>?
        </v-card-text>
        <v-card-actions>
          <v-spacer />
          <v-btn variant="text" @click="purgeDialog = false">Cancel</v-btn>
          <v-btn color="error" variant="flat" :loading="purging" @click="confirmPurge">Purge</v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>

    <v-snackbar v-model="snackbar" :color="snackColor" timeout="3000">
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
