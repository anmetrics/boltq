<template>
  <div>
    <header class="page-header">
      <div>
        <h1 class="page-title">Cluster</h1>
        <p class="page-subtitle">Raft consensus and node topology</p>
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

    <!-- Cluster disabled -->
    <div v-if="!clusterEnabled" class="modern-card pa-12 text-center">
      <v-icon size="48" color="grey-lighten-1" class="mb-4">mdi-server-network-off</v-icon>
      <h3 class="text-h6 font-weight-bold mb-2">Cluster Mode Disabled</h3>
      <p class="text-body-2 text-medium-emphasis mb-4">
        Enable clustering in the configuration to utilize Raft-based durable quorum queues.
      </p>
      <code class="config-badge">cluster.enabled: false</code>
    </div>

    <!-- Cluster enabled -->
    <template v-else>
      <!-- Stats cards -->
      <v-row class="mb-4">
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">State</div>
            <v-chip :color="stateColor" variant="tonal" size="small" class="font-weight-bold">
              {{ cluster?.state || 'Unknown' }}
            </v-chip>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Node ID</div>
            <div class="mono text-body-1 font-weight-bold" style="color: var(--primary)">{{ cluster?.node_id }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Current Term</div>
            <div class="metric-value" style="font-size: 1.5rem">{{ cluster?.term || 0 }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="modern-card pa-5">
            <div class="metric-label mb-2">Commit Index</div>
            <div class="metric-value" style="font-size: 1.5rem">{{ cluster?.last_index || 0 }}</div>
          </div>
        </v-col>
      </v-row>

      <!-- Leader -->
      <div class="modern-card pa-5 mb-4">
        <div class="d-flex align-center mb-3">
          <v-icon size="18" color="warning" class="mr-2">mdi-crown</v-icon>
          <span class="card-title">Leader</span>
        </div>
        <div class="d-flex align-center ga-3">
          <v-chip color="success" size="small" variant="tonal" class="font-weight-bold">
            {{ cluster?.leader_id }}
          </v-chip>
          <span class="mono text-body-2 text-medium-emphasis">{{ cluster?.leader }}</span>
        </div>
      </div>

      <!-- Peers -->
      <div class="modern-card-flat mb-4">
        <div class="pa-5 pb-3">
          <div class="d-flex align-center">
            <v-icon size="18" color="info" class="mr-2">mdi-lan</v-icon>
            <span class="card-title">Peers ({{ cluster?.peers?.length || 0 }})</span>
          </div>
        </div>
        <v-table class="premium-table" density="compact">
          <thead>
            <tr>
              <th>Node</th>
              <th>Address</th>
              <th>Role</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="peer in parsedPeers" :key="peer.raw">
              <td class="mono font-weight-medium">{{ peer.id }}</td>
              <td class="mono text-medium-emphasis">{{ peer.addr }}</td>
              <td>
                <v-chip
                  :color="peer.id === cluster?.leader_id ? 'success' : 'default'"
                  size="x-small"
                  variant="tonal"
                  class="font-weight-bold"
                >
                  {{ peer.id === cluster?.leader_id ? 'Leader' : 'Follower' }}
                </v-chip>
              </td>
            </tr>
          </tbody>
        </v-table>
      </div>

      <!-- Join / Leave -->
      <v-row>
        <v-col cols="12" md="6">
          <div class="modern-card pa-5">
            <div class="d-flex align-center mb-4">
              <v-icon size="18" color="success" class="mr-2">mdi-plus-network</v-icon>
              <span class="card-title">Join Node</span>
            </div>
            <v-text-field
              v-model="joinNodeId"
              label="Node ID"
              placeholder="node4"
              class="mb-2"
            />
            <v-text-field
              v-model="joinAddr"
              label="Raft Address"
              placeholder="10.0.0.4:9100"
              class="mb-3"
            />
            <v-btn
              color="primary"
              variant="flat"
              rounded="lg"
              block
              :loading="joining"
              :disabled="!joinNodeId || !joinAddr"
              @click="joinNode"
            >
              Join Cluster
            </v-btn>
          </div>
        </v-col>
        <v-col cols="12" md="6">
          <div class="modern-card pa-5">
            <div class="d-flex align-center mb-4">
              <v-icon size="18" color="error" class="mr-2">mdi-minus-network</v-icon>
              <span class="card-title">Remove Node</span>
            </div>
            <v-text-field
              v-model="leaveNodeId"
              label="Node ID"
              placeholder="node4"
              class="mb-3"
            />
            <v-btn
              color="error"
              variant="flat"
              rounded="lg"
              block
              :loading="leaving"
              :disabled="!leaveNodeId"
              @click="leaveNode"
            >
              Remove from Cluster
            </v-btn>
          </div>
        </v-col>
      </v-row>
    </template>

    <v-snackbar v-model="snackbar" :color="snackColor" timeout="3000" rounded="lg">
      {{ snackMessage }}
    </v-snackbar>
  </div>
</template>

<script setup lang="ts">
const api = useApi()
const loading = ref(false)
const data = ref<any>(null)
const joinNodeId = ref('')
const joinAddr = ref('')
const leaveNodeId = ref('')
const joining = ref(false)
const leaving = ref(false)
const snackbar = ref(false)
const snackMessage = ref('')
const snackColor = ref('success')

const clusterEnabled = computed(() => data.value?.enabled === true)
const cluster = computed(() => data.value?.cluster)
const stateColor = computed(() => {
  const s = cluster.value?.state
  if (s === 'Leader') return 'success'
  if (s === 'Follower') return 'info'
  if (s === 'Candidate') return 'warning'
  return 'grey'
})

const parsedPeers = computed(() => {
  const peers = cluster.value?.peers || []
  return peers.map((p: string) => {
    const parts = p.split('@')
    return { raw: p, id: parts[0], addr: parts[1] || p }
  })
})

async function joinNode() {
  joining.value = true
  try {
    await api.clusterJoin(joinNodeId.value, joinAddr.value)
    snackMessage.value = `Node ${joinNodeId.value} joined cluster`
    snackColor.value = 'success'
    snackbar.value = true
    joinNodeId.value = ''
    joinAddr.value = ''
    await refresh()
  } catch (e: any) {
    snackMessage.value = e.data?.error || 'Join failed'
    snackColor.value = 'error'
    snackbar.value = true
  } finally {
    joining.value = false
  }
}

async function leaveNode() {
  leaving.value = true
  try {
    await api.clusterLeave(leaveNodeId.value)
    snackMessage.value = `Node ${leaveNodeId.value} removed from cluster`
    snackColor.value = 'success'
    snackbar.value = true
    leaveNodeId.value = ''
    await refresh()
  } catch (e: any) {
    snackMessage.value = e.data?.error || 'Leave failed'
    snackColor.value = 'error'
    snackbar.value = true
  } finally {
    leaving.value = false
  }
}

async function refresh() {
  loading.value = true
  try { data.value = await api.getClusterStatus() } catch {}
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
.card-title {
  font-size: 0.95rem;
  font-weight: 600;
  color: var(--text-primary);
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
.ga-3 { gap: 12px; }
</style>
