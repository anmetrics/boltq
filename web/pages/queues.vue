<template>
  <div class="premium-dashboard">
    <header class="dashboard-header d-flex align-center mb-10">
      <div>
        <h1 class="text-h4 font-weight-black gradient-text-primary mb-1">Queue Management</h1>
        <div class="text-caption text-muted font-weight-bold letter-spacing-1">MONITORING WORKLOAD AND PERFORMANCE</div>
      </div>
      <v-spacer />
      <v-btn
        variant="tonal"
        color="primary"
        rounded="lg"
        prepend-icon="mdi-refresh"
        :loading="loading"
        @click="refresh"
      >
        REFRESH
      </v-btn>
    </header>

    <div class="glass-card table-glass pa-0">

<v-table class="premium-table">
        <thead>
          <tr>
            <th>Queue Name</th>
            <th class="text-right">Messages</th>
            <th class="text-right">Dead Letters</th>
            <th class="text-right">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="rows.length === 0">
            <td colspan="4" class="text-center pa-8" style="opacity: 0.4">
              <v-icon size="48" class="mb-2" style="opacity: 0.3">mdi-tray-remove</v-icon>
              <div>No queues created yet</div>
              <div class="text-caption mt-1">Publish a message via TCP to create a queue</div>
            </td>
          </tr>
          <tr v-for="q in rows" :key="q.name">
            <td>
              <span class="mono font-weight-medium">{{ q.name }}</span>
            </td>
            <td class="text-right">
              <v-chip :color="q.messages > 0 ? 'primary' : 'default'" size="small" variant="flat">
                {{ q.messages.toLocaleString() }}
              </v-chip>
            </td>
            <td class="text-right">
              <v-chip v-if="q.deadLetters > 0" color="error" size="small" variant="flat">
                {{ q.deadLetters.toLocaleString() }}
              </v-chip>
              <span v-else class="mono" style="opacity: 0.3">0</span>
            </td>
            <td class="text-right">
              <v-btn
                size="x-small"
                variant="tonal"
                color="warning"
                :disabled="q.messages === 0"
                @click="purge(q.name)"
              >
                Purge
              </v-btn>
            </td>
          </tr>
        </tbody>
      </v-table>
      <div class="pa-4 d-flex align-center px-6" style="border-top: 1px solid var(--glass-border)">
        <v-icon size="16" class="mr-2 text-muted">mdi-clock-outline</v-icon>
        <span class="text-caption text-muted font-weight-bold">
          PENDING ACKS: {{ stats?.PendingCount || 0 }}
        </span>
        <v-spacer />
        <span class="text-caption text-muted font-weight-bold">
          TOTAL ACTIVE: {{ rows.length }}
        </span>
      </div>
    </div>

    <!-- Purge confirmation dialog -->
    <v-dialog v-model="purgeDialog" max-width="400">
      <v-card color="surface">
        <v-card-title>Purge Queue</v-card-title>
        <v-card-text>
          Are you sure you want to purge all messages from
          <strong class="mono">{{ purgeTarget }}</strong>?
          This action cannot be undone.
        </v-card-text>
        <v-card-actions>
          <v-spacer />
          <v-btn variant="text" @click="purgeDialog = false">Cancel</v-btn>
          <v-btn color="warning" variant="flat" :loading="purging" @click="confirmPurge">Purge</v-btn>
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
  const queues = stats.value?.Queues || {}
  const dls = stats.value?.DeadLetters || {}
  return Object.entries(queues).map(([name, messages]) => ({
    name,
    messages: messages as number,
    deadLetters: (dls[name + '_dead_letter'] || 0) as number,
  })).sort((a, b) => a.name.localeCompare(b.name))
})

function purge(name: string) {
  purgeTarget.value = name
  purgeDialog.value = true
}

async function confirmPurge() {
  purging.value = true
  try {
    const result = await api.purgeQueue(purgeTarget.value)
    snackMessage.value = `Purged ${result.purged_count} messages from ${purgeTarget.value}`
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
  try {
    stats.value = await api.getStats()
  } catch {}
  loading.value = false
}

let timer: ReturnType<typeof setInterval>
onMounted(() => { refresh(); timer = setInterval(refresh, 5000) })
onUnmounted(() => clearInterval(timer))
</script>
