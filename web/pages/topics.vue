<template>
  <div>
    <div class="d-flex align-center mb-6">
      <div>
        <h1 class="text-h4 font-weight-bold">Pub/Sub Topics</h1>
        <p class="text-body-2 mt-1" style="opacity: 0.5">Active topics and subscriber counts</p>
      </div>
      <v-spacer />
      <v-btn icon="mdi-refresh" variant="text" size="small" :loading="loading" @click="refresh" />
    </div>

    <v-card class="data-table-card" color="surface">
      <v-table hover>
        <thead>
          <tr>
            <th>Topic Name</th>
            <th class="text-right">Subscribers</th>
            <th class="text-right">Status</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="rows.length === 0">
            <td colspan="3" class="text-center pa-8" style="opacity: 0.4">
              <v-icon size="48" class="mb-2" style="opacity: 0.3">mdi-broadcast-off</v-icon>
              <div>No topics created yet</div>
              <div class="text-caption mt-1">Subscribe to a topic via TCP to see it here</div>
            </td>
          </tr>
          <tr v-for="t in rows" :key="t.name">
            <td>
              <span class="mono font-weight-medium">{{ t.name }}</span>
            </td>
            <td class="text-right">
              <v-chip :color="t.subscribers > 0 ? 'secondary' : 'default'" size="small" variant="flat">
                {{ t.subscribers }}
              </v-chip>
            </td>
            <td class="text-right">
              <v-chip :color="t.subscribers > 0 ? 'success' : 'grey'" size="x-small" variant="flat">
                {{ t.subscribers > 0 ? 'Active' : 'Idle' }}
              </v-chip>
            </td>
          </tr>
        </tbody>
      </v-table>
      <div class="pa-4 text-caption" style="opacity: 0.3; border-top: 1px solid rgba(255,255,255,0.06)">
        Total topics: {{ rows.length }}
      </div>
    </v-card>
  </div>
</template>

<script setup lang="ts">
const api = useApi()
const loading = ref(false)
const stats = ref<any>(null)

const rows = computed(() => {
  const topics = stats.value?.Topics || {}
  return Object.entries(topics).map(([name, subscribers]) => ({
    name,
    subscribers: subscribers as number,
  })).sort((a, b) => a.name.localeCompare(b.name))
})

async function refresh() {
  loading.value = true
  try { stats.value = await api.getStats() } catch {}
  loading.value = false
}

let timer: ReturnType<typeof setInterval>
onMounted(() => { refresh(); timer = setInterval(refresh, 5000) })
onUnmounted(() => clearInterval(timer))
</script>
