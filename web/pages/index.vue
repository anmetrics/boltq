<template>
  <div class="obsidian-dashboard">
    <!-- Header -->
    <div class="d-flex align-center mb-8">
      <div>
        <h1 class="text-h4 font-weight-black gradient-text">Dashboard</h1>
        <div class="d-flex align-center mt-1">
          <div class="status-indicator mr-2" :class="{ online: isOnline }" />
          <span
            class="text-caption font-weight-bold"
            :class="isOnline ? 'text-cyan' : 'text-red'"
          >
            {{ isOnline ? "SYSTEM OPERATIONAL" : "SYSTEM OFFLINE" }}
          </span>
          <span class="mx-2 text-grey-darken-1">|</span>
          <span class="text-caption text-grey">{{
            overview?.uptime_ms ? formatUptime(overview.uptime_ms) : "UPTIME --"
          }}</span>
        </div>
      </div>
      <v-spacer />
      <div class="refresh-control d-flex align-center">
        <span class="text-caption mr-3 text-grey">Auto-sync: 5s</span>
        <v-btn
          icon="mdi-refresh"
          variant="text"
          size="small"
          :loading="loading"
          @click="refresh"
          class="neon-btn"
        />
      </div>
    </div>

    <!-- Top Metrics -->
    <v-row class="mb-6">
      <v-col
        v-for="card in metricCards"
        :key="card.label"
        cols="12"
        sm="6"
        md="4"
        lg="2"
      >
        <div class="glass-card metric-card">
          <div class="d-flex align-center justify-space-between mb-2">
            <v-icon :color="card.color" size="20">{{ card.icon }}</v-icon>
            <span
              class="text-caption font-weight-bold"
              :style="{ color: card.color }"
              >{{ card.trend }}</span
            >
          </div>
          <div class="metric-value font-weight-black">
            {{ formatNumber(card.value) }}
          </div>
          <div class="metric-label">{{ card.label }}</div>
          <div
            class="glow-bg"
            :style="{ background: card.color, opacity: 0.05 }"
          />
        </div>
      </v-col>
    </v-row>

    <!-- Main Charts & Info -->
    <v-row>
      <!-- Throughput Chart -->
      <v-col cols="12" lg="8">
        <div class="glass-card chart-container h-100">
          <div class="d-flex align-center justify-space-between mb-6">
            <h3 class="text-subtitle-1 font-weight-bold d-flex align-center">
              <v-icon size="18" class="mr-2 text-cyan">mdi-chart-line</v-icon>
              Throughput Real-time
            </h3>
            <div class="d-flex align-center">
              <div class="chart-legend mr-4">
                <span class="dot" style="background: #00f2ff" /> Published
              </div>
              <div class="chart-legend">
                <span class="dot" style="background: #00ff88" /> Consumed
              </div>
            </div>
          </div>
          <div class="chart-wrapper">
            <v-chart class="chart" :option="throughputOption" autoresize />
          </div>
        </div>
      </v-col>

      <!-- Storage & System -->
      <v-col cols="12" lg="4">
        <div class="d-flex flex-column gap-y-4 h-100">
          <!-- Storage Monitor -->
          <div class="glass-card monitor-card">
            <h3
              class="text-subtitle-1 font-weight-bold mb-4 d-flex align-center"
            >
              <v-icon size="18" class="mr-2 text-purple">mdi-database</v-icon>
              Storage Integrity
            </h3>
            <div class="d-flex align-center justify-space-between mb-2">
              <span class="text-caption text-grey"
                >WAL Size ({{
                  overview?.storage?.mode?.toUpperCase() || "--"
                }})</span
              >
              <span class="text-body-2 font-weight-bold">{{
                formatBytes(overview?.storage?.size || 0)
              }}</span>
            </div>
            <v-progress-linear
              :model-value="storagePercent"
              color="purple-accent-2"
              height="8"
              rounded
              class="mb-4 shadow-progress"
            />
            <div
              class="d-flex align-center justify-space-between text-caption text-grey"
            >
              <span>Health: Stable</span>
              <span
                >Threshold:
                {{
                  formatBytes(overview?.storage?.compaction_threshold || 0)
                }}</span
              >
            </div>
          </div>

          <!-- Resource Monitor -->
          <div class="glass-card monitor-card">
            <h3
              class="text-subtitle-1 font-weight-bold mb-4 d-flex align-center"
            >
              <v-icon size="18" class="mr-2 text-amber">mdi-chip</v-icon>
              System Resources
            </h3>
            <v-row dense>
              <v-col cols="6">
                <div class="res-item">
                  <div class="text-caption text-grey mb-1">
                    Cores (Routines)
                  </div>
                  <div class="text-h6 font-weight-black text-amber">
                    {{ overview?.system?.goroutines || 0 }}
                  </div>
                </div>
              </v-col>
              <v-col cols="6">
                <div class="res-item">
                  <div class="text-caption text-grey mb-1">Heap Memory</div>
                  <div class="text-h6 font-weight-black text-amber">
                    {{ formatBytes(overview?.system?.memory || 0) }}
                  </div>
                </div>
              </v-col>
            </v-row>
          </div>

          <!-- Cluster Status Quick View -->
          <div class="glass-card monitor-card flex-grow-1">
            <h3
              class="text-subtitle-1 font-weight-bold mb-4 d-flex align-center"
            >
              <v-icon size="18" class="mr-2 text-blue"
                >mdi-server-network</v-icon
              >
              Cluster Topology
            </h3>
            <div v-if="!overview?.cluster?.enabled" class="text-center py-4">
              <v-chip color="grey-darken-3" size="small" variant="flat"
                >Standalone Mode</v-chip
              >
            </div>
            <div v-else>
              <div class="d-flex align-center justify-space-between mb-4">
                <v-chip
                  :color="isLeader ? 'success' : 'info'"
                  size="small"
                  variant="flat"
                  class="text-uppercase font-weight-bold"
                >
                  {{ overview.cluster.cluster?.state }}
                </v-chip>
                <span class="text-caption mono text-grey-darken-1">{{
                  overview.cluster.cluster?.node_id
                }}</span>
              </div>
              <div class="topology-mini">
                <div class="text-caption text-grey mb-2">
                  Network Peers:
                  {{ overview.cluster.cluster?.peers?.length || 0 }}
                </div>
                <div class="text-caption text-grey">
                  Term: {{ overview.cluster.cluster?.term || 0 }}
                </div>
              </div>
            </div>
          </div>
        </div>
      </v-col>
    </v-row>

    <!-- Bottom Lists -->
    <v-row class="mt-4">
      <v-col cols="12" md="6">
        <div class="glass-card table-card h-100">
          <div class="d-flex align-center justify-space-between mb-4">
            <h3 class="text-subtitle-1 font-weight-bold">
              <v-icon size="18" class="mr-2 text-cyan">mdi-tray-full</v-icon>
              Active Work Queues
            </h3>
            <v-btn
              to="/queues"
              variant="text"
              size="small"
              color="cyan"
              density="compact"
              >View All</v-btn
            >
          </div>
          <v-table density="compact" class="obsidian-table">
            <thead>
              <tr>
                <th class="text-left">Queue Name</th>
                <th class="text-right">Messages</th>
                <th class="text-right">DLQ</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="queueRows.length === 0">
                <td colspan="3" class="text-center pa-6 text-grey">
                  No active queues
                </td>
              </tr>
              <tr v-for="q in queueRows.slice(0, 5)" :key="q.name">
                <td class="mono text-cyan-accent-1">{{ q.name }}</td>
                <td class="text-right mono font-weight-bold">
                  {{ formatNumber(q.messages) }}
                </td>
                <td class="text-right mono">
                  <span
                    v-if="q.deadLetters > 0"
                    class="text-red font-weight-bold"
                    >{{ q.deadLetters }}</span
                  >
                  <span v-else class="text-grey-darken-2">0</span>
                </td>
              </tr>
            </tbody>
          </v-table>
        </div>
      </v-col>

      <v-col cols="12" md="6">
        <div class="glass-card table-card h-100">
          <div class="d-flex align-center justify-space-between mb-4">
            <h3 class="text-subtitle-1 font-weight-bold">
              <v-icon size="18" class="mr-2 text-green">mdi-broadcast</v-icon>
              Global Topics
            </h3>
            <v-btn
              to="/topics"
              variant="text"
              size="small"
              color="green"
              density="compact"
              >View All</v-btn
            >
          </div>
          <v-table density="compact" class="obsidian-table">
            <thead>
              <tr>
                <th class="text-left">Topic Name</th>
                <th class="text-right">Fans</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="topicRows.length === 0">
                <td colspan="2" class="text-center pa-6 text-grey">
                  No active topics
                </td>
              </tr>
              <tr v-for="t in topicRows.slice(0, 5)" :key="t.name">
                <td class="mono text-green-accent-1">{{ t.name }}</td>
                <td class="text-right">
                  <v-chip
                    size="x-small"
                    color="green"
                    variant="tonal"
                    class="font-weight-bold"
                  >
                    {{ t.subscribers }} subs
                  </v-chip>
                </td>
              </tr>
            </tbody>
          </v-table>
        </div>
      </v-col>
    </v-row>
  </div>
</template>

<script setup lang="ts">
import { useIntervalFn } from "@vueuse/core";

const api = useApi();
const { isOnline, setOnline } = useServerStatus();

const loading = ref(false);
const overview = ref<any>(null);
const history = ref<{ time: string; published: number; consumed: number }[]>(
  [],
);
const MAX_HISTORY = 20;

const isLeader = computed(
  () => overview.value?.cluster?.cluster?.state === "Leader",
);

const metricCards = computed(() => {
  const m = overview.value?.metrics || {};
  return [
    {
      label: "Published",
      value: m.messages_published || 0,
      icon: "mdi-upload",
      color: "#00f2ff",
      trend: "+Live",
    },
    {
      label: "Consumed",
      value: m.messages_consumed || 0,
      icon: "mdi-download",
      color: "#00ff88",
      trend: "+Live",
    },
    {
      label: "Pending",
      value: overview.value?.stats?.PendingCount || 0,
      icon: "mdi-clock-outline",
      color: "#ffea00",
      trend: "Queue",
    },
    {
      label: "Acked",
      value: m.messages_acked || 0,
      icon: "mdi-shield-check",
      color: "#00e676",
      trend: "Safe",
    },
    {
      label: "Nacked",
      value: m.messages_nacked || 0,
      icon: "mdi-alert-circle",
      color: "#f44336",
      trend: "Retry",
    },
    {
      label: "Dead Letter",
      value: m.dead_letter_count || 0,
      icon: "mdi-skull",
      color: "#ff5252",
      trend: "Alert",
    },
  ];
});

const storagePercent = computed(() => {
  const size = overview.value?.storage?.size || 0;
  const threshold = overview.value?.storage?.compaction_threshold || 104857600;
  return Math.min((size / threshold) * 100, 100);
});

const queueRows = computed(() => {
  const queues = overview.value?.stats?.Queues || {};
  const dls = overview.value?.stats?.DeadLetters || {};
  return Object.entries(queues).map(([name, messages]) => ({
    name,
    messages: messages as number,
    deadLetters: (dls[name + "_dead_letter"] || 0) as number,
  }));
});

const topicRows = computed(() => {
  const topics = overview.value?.stats?.Topics || {};
  return Object.entries(topics).map(([name, subscribers]) => ({
    name,
    subscribers: subscribers as number,
  }));
});

// Throughput Chart Option
const throughputOption = computed(() => {
  return {
    backgroundColor: "transparent",
    grid: {
      left: "3%",
      right: "4%",
      bottom: "3%",
      top: "10%",
      containLabel: true,
    },
    tooltip: {
      trigger: "axis",
      backgroundColor: "#1a1a20",
      borderColor: "#333",
      textStyle: { color: "#fff" },
    },
    xAxis: {
      type: "category",
      data: history.value.map((h) => h.time),
      axisLine: { lineStyle: { color: "#333" } },
      axisLabel: { color: "#666" },
    },
    yAxis: {
      type: "value",
      splitLine: { lineStyle: { color: "#1a1a20" } },
      axisLabel: { color: "#666" },
    },
    series: [
      {
        name: "Published",
        type: "line",
        smooth: true,
        showSymbol: false,
        data: history.value.map((h) => h.published),
        lineStyle: { width: 3, color: "#00f2ff" },
        areaStyle: {
          opacity: 0.1,
          color: {
            type: "linear",
            x: 0,
            y: 0,
            x2: 0,
            y2: 1,
            colorStops: [
              { offset: 0, color: "#00f2ff" },
              { offset: 1, color: "transparent" },
            ],
          },
        },
      },
      {
        name: "Consumed",
        type: "line",
        smooth: true,
        showSymbol: false,
        data: history.value.map((h) => h.consumed),
        lineStyle: { width: 3, color: "#00ff88" },
        areaStyle: {
          opacity: 0.1,
          color: {
            type: "linear",
            x: 0,
            y: 0,
            x2: 0,
            y2: 1,
            colorStops: [
              { offset: 0, color: "#00ff88" },
              { offset: 1, color: "transparent" },
            ],
          },
        },
      },
    ],
  };
});

function formatNumber(n: number): string {
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "K";
  return n.toLocaleString();
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + " " + sizes[i];
}

function formatUptime(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  return `${h}h ${m}m ${s}s`;
}

async function refresh() {
  loading.value = true;
  try {
    const data = await api.getOverview();

    // Update history
    const now = new Date().toLocaleTimeString([], {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    });
    const lastMetrics = overview.value?.metrics;
    if (lastMetrics) {
      const pubSec = Math.max(
        0,
        data.metrics.messages_published - lastMetrics.messages_published,
      );
      const conSec = Math.max(
        0,
        data.metrics.messages_consumed - lastMetrics.messages_consumed,
      );
      history.value.push({ time: now, published: pubSec, consumed: conSec });
      if (history.value.length > MAX_HISTORY) history.value.shift();
    }

    overview.value = data;
    setOnline(true);
  } catch {
    setOnline(false);
  } finally {
    loading.value = false;
  }
}

useIntervalFn(refresh, 5000);

onMounted(() => {
  refresh();
});
</script>

<style lang="scss">
.obsidian-dashboard {
  min-height: 100vh;
  padding: 24px;
  color: #f1f1f1;

  .gradient-text {
    background: linear-gradient(135deg, #f1f1f1 0%, #a1a1a1 100%);
    -webkit-background-clip: text;
    -webkit-text-fill-color: transparent;
  }

  .status-indicator {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: #555;
    box-shadow: 0 0 10px rgba(255, 255, 255, 0.1);

    &.online {
      background: #00f2ff;
      box-shadow: 0 0 10px rgba(0, 242, 255, 0.5);
    }
  }

  .glass-card {
    background: rgba(18, 18, 21, 0.6);
    backdrop-filter: blur(12px);
    border: 1px solid rgba(255, 255, 255, 0.05);
    border-radius: 16px;
    padding: 20px;
    position: relative;
    overflow: hidden;
    transition:
      transform 0.2s,
      border-color 0.2s;

    &:hover {
      border-color: rgba(255, 255, 255, 0.1);
    }
  }

  .metric-card {
    .metric-value {
      font-size: 24px;
      line-height: 1.2;
      margin-top: 8px;
    }
    .metric-label {
      font-size: 11px;
      text-transform: uppercase;
      letter-spacing: 1px;
      opacity: 0.5;
      margin-top: 4px;
    }
    .glow-bg {
      position: absolute;
      top: 0;
      left: 0;
      right: 0;
      bottom: 0;
      z-index: -1;
    }
  }

  .chart-container {
    .chart-wrapper {
      height: 300px;
    }
    .chart-legend {
      font-size: 11px;
      color: #666;
      display: flex;
      align-center: center;
      .dot {
        width: 8px;
        height: 8px;
        border-radius: 50%;
        margin-right: 6px;
      }
    }
  }

  .monitor-card {
    padding: 16px;
    .shadow-progress {
      box-shadow: 0 0 15px rgba(191, 0, 255, 0.2);
    }
    .res-item {
      background: rgba(0, 0, 0, 0.2);
      padding: 12px;
      border-radius: 12px;
    }
  }

  .obsidian-table {
    background: transparent !important;
    color: #f1f1f1 !important;

    th {
      font-size: 11px !important;
      text-transform: uppercase;
      letter-spacing: 1px;
      color: #555 !important;
      border-bottom: 1px solid rgba(255, 255, 255, 0.05) !important;
    }

    td {
      font-size: 13px !important;
      border-bottom: 1px solid rgba(255, 255, 255, 0.02) !important;
    }

    tr:hover td {
      background: rgba(255, 255, 255, 0.01) !important;
    }
  }

  .mono {
    font-family: "JetBrains Mono", "Fira Code", monospace;
  }

  .gap-y-4 {
    row-gap: 16px;
  }
}

.neon-btn {
  &:hover {
    color: #00f2ff;
    text-shadow: 0 0 10px rgba(0, 242, 255, 0.5);
  }
}
</style>
