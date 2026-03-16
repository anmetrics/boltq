<template>
  <div class="premium-dashboard">
    <header class="dashboard-header d-flex align-center mb-10">
      <div>
        <h1 class="text-h4 font-weight-black gradient-text-primary mb-1">Cluster Management</h1>
        <div class="text-caption text-muted font-weight-bold letter-spacing-1">RAFT CONSENSUS AND TOPOLOGY</div>
      </div>
      <v-spacer />
      <v-btn
        variant="tonal"
        color="info"
        rounded="lg"
        prepend-icon="mdi-refresh"
        :loading="loading"
        @click="refresh"
      >
        REFRESH
      </v-btn>
    </header>

    <div v-if="!clusterEnabled" class="glass-card pa-12 text-center">
      <v-icon size="64" class="mb-4" style="opacity: 0.2">mdi-server-network-off</v-icon>
      <h3 class="text-h5 font-weight-bold mb-2">Cluster Mode Disabled</h3>
      <p class="text-body-2 text-muted mb-4">
        Enable clustering in the configuration to utilize Raft-based durable quorum queues.
      </p>
      <div class="d-inline-flex pa-2 px-4 bg-surface-subtle rounded-pill mono text-caption text-muted">
        cluster.enabled: false
      </div>
    </div>

    <!-- Cluster enabled -->
    <template v-else>
      <v-row class="mb-4">
        <v-col cols="12" sm="6" md="3">
          <div class="glass-card pa-5">
            <div class="metric-label mb-2">OPERATIONAL STATE</div>
            <v-chip :color="stateColor" variant="flat" size="small" class="font-weight-black px-4">
              {{ cluster?.state || 'UNKNOWN' }}
            </v-chip>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="glass-card pa-5">
            <div class="metric-label mb-2">LOCAL NODE IDENTIFIER</div>
            <div class="mono text-body-1 font-weight-bold text-primary">{{ cluster?.node_id }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="glass-card pa-5">
            <div class="metric-label mb-2">CURRENT TERM</div>
            <div class="text-h5 font-weight-black gradient-text-primary">{{ cluster?.term || 0 }}</div>
          </div>
        </v-col>
        <v-col cols="12" sm="6" md="3">
          <div class="glass-card pa-5">
            <div class="metric-label mb-2">COMMIT INDEX</div>
            <div class="text-h5 font-weight-black gradient-text-primary">{{ cluster?.last_index || 0 }}</div>
          </div>
        </v-col>
      </v-row>

      <!-- Leader info -->
      <v-card class="data-table-card mb-4" color="surface">
        <v-card-title class="text-body-1 font-weight-bold pa-4 pb-2">
          <v-icon size="18" class="mr-2">mdi-crown</v-icon>
          Leader
        </v-card-title>
        <v-card-text>
          <div class="d-flex align-center">
            <v-chip color="success" size="small" variant="flat" class="mr-3">
              {{ cluster?.leader_id }}
            </v-chip>
            <span class="mono text-body-2" style="opacity: 0.5">{{ cluster?.leader }}</span>
          </div>
        </v-card-text>
      </v-card>

      <!-- Peers -->
      <v-card class="data-table-card mb-4" color="surface">
        <v-card-title class="text-body-1 font-weight-bold pa-4 pb-2">
          <v-icon size="18" class="mr-2">mdi-lan</v-icon>
          Peers ({{ cluster?.peers?.length || 0 }})
        </v-card-title>
        <v-table density="compact" hover>
          <thead>
            <tr>
              <th>Node</th>
              <th>Address</th>
              <th>Role</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="peer in parsedPeers" :key="peer.raw">
              <td class="mono">{{ peer.id }}</td>
              <td class="mono" style="opacity: 0.5">{{ peer.addr }}</td>
              <td>
                <v-chip
                  :color="peer.id === cluster?.leader_id ? 'success' : 'default'"
                  size="x-small"
                  variant="flat"
                >
                  {{ peer.id === cluster?.leader_id ? 'Leader' : 'Follower' }}
                </v-chip>
              </td>
            </tr>
          </tbody>
        </v-table>
      </v-card>

      <!-- Join / Leave -->
      <v-row>
        <v-col cols="12" md="6">
          <v-card class="data-table-card" color="surface">
            <v-card-title class="text-body-1 font-weight-bold pa-4 pb-2">
              <v-icon size="18" class="mr-2">mdi-plus-network</v-icon>
              Join Node
            </v-card-title>
            <v-card-text>
              <v-text-field
                v-model="joinNodeId"
                label="Node ID"
                placeholder="node4"
                variant="outlined"
                density="compact"
                class="mb-2"
              />
              <v-text-field
                v-model="joinAddr"
                label="Raft Address"
                placeholder="10.0.0.4:9100"
                variant="outlined"
                density="compact"
                class="mb-2"
              />
              <v-btn
                color="primary"
                variant="flat"
                block
                :loading="joining"
                :disabled="!joinNodeId || !joinAddr"
                @click="joinNode"
              >
                Join Cluster
              </v-btn>
            </v-card-text>
          </v-card>
        </v-col>
        <v-col cols="12" md="6">
          <v-card class="data-table-card" color="surface">
            <v-card-title class="text-body-1 font-weight-bold pa-4 pb-2">
              <v-icon size="18" class="mr-2">mdi-minus-network</v-icon>
              Remove Node
            </v-card-title>
            <v-card-text>
              <v-text-field
                v-model="leaveNodeId"
                label="Node ID"
                placeholder="node4"
                variant="outlined"
                density="compact"
                class="mb-2"
              />
              <v-btn
                color="error"
                variant="flat"
                block
                :loading="leaving"
                :disabled="!leaveNodeId"
                @click="leaveNode"
              >
                Remove from Cluster
              </v-btn>
            </v-card-text>
          </v-card>
        </v-col>
      </v-row>
    </template>

    <v-snackbar v-model="snackbar" :color="snackColor" timeout="3000">
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
