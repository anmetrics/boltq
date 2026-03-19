<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Queues</h1>
        <p class="page-subtitle">Monitor and manage message queues</p>
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
            <th>Queue Name</th>
            <th class="text-right">Messages</th>
            <th class="text-right">Dead Letters</th>
            <th class="text-right">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="rows.length === 0">
            <td colspan="4" class="text-center pa-10">
              <v-icon size="40" color="grey-lighten-1" class="mb-3">mdi-tray-remove</v-icon>
              <div class="text-body-2 text-medium-emphasis">No queues created yet</div>
              <div class="text-caption text-medium-emphasis mt-1">Publish a message via TCP to create a queue</div>
            </td>
          </tr>
          <tr v-for="q in rows" :key="q.name">
            <td>
              <div class="d-flex align-center">
                <v-icon size="16" color="primary" class="mr-2" style="opacity: 0.5">mdi-tray-full</v-icon>
                <span class="mono font-weight-medium">{{ q.name }}</span>
              </div>
            </td>
            <td class="text-right">
              <v-chip :color="q.messages > 0 ? 'primary' : 'default'" size="small" variant="tonal" class="font-weight-bold">
                {{ q.messages.toLocaleString() }}
              </v-chip>
            </td>
            <td class="text-right">
              <v-chip v-if="q.deadLetters > 0" color="error" size="small" variant="tonal" class="font-weight-bold">
                {{ q.deadLetters.toLocaleString() }}
              </v-chip>
              <span v-else class="text-medium-emphasis">0</span>
            </td>
            <td class="text-right">
              <v-btn
                size="x-small"
                variant="tonal"
                color="warning"
                rounded="lg"
                :disabled="q.messages === 0"
                @click="purge(q.name)"
              >
                Purge
              </v-btn>
            </td>
          </tr>
        </tbody>
      </v-table>
      <div class="table-footer">
        <div class="d-flex align-center">
          <v-icon size="14" class="mr-2 text-medium-emphasis">mdi-clock-outline</v-icon>
          <span class="text-caption text-medium-emphasis font-weight-medium">
            Pending Acks: {{ stats?.PendingCount || 0 }}
          </span>
        </div>
        <span class="text-caption text-medium-emphasis font-weight-medium">
          Total: {{ rows.length }} queues
        </span>
      </div>
    </div>

    <v-dialog v-model="purgeDialog" max-width="420">
      <v-card rounded="xl">
        <v-card-title class="text-body-1 font-weight-bold pt-5">Purge Queue</v-card-title>
        <v-card-text class="text-body-2">
          Are you sure you want to purge all messages from
          <strong class="mono">{{ purgeTarget }}</strong>?
          This action cannot be undone.
        </v-card-text>
        <v-card-actions class="pa-4 pt-0">
          <v-spacer />
          <v-btn variant="text" @click="purgeDialog = false">Cancel</v-btn>
          <v-btn color="warning" variant="flat" rounded="lg" :loading="purging" @click="confirmPurge">Purge</v-btn>
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
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 12px 16px;
  border-top: 1px solid var(--border-color);
}
</style>
