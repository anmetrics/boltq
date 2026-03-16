<template>
  <div class="premium-dashboard">
    <!-- Header Section -->
    <header class="dashboard-header d-flex align-center mb-10">
      <div>
        <h1 class="text-h3 font-weight-black gradient-text-primary mb-1">
          Infrastructure Control
        </h1>
        <div class="d-flex align-center">
          <div
            class="status-indicator-group d-flex align-center px-3 py-1 bg-surface-subtle rounded-pill"
          >
            <span
              class="status-dot mr-2"
              :class="isOnline ? 'online' : 'offline'"
            />
            <span
              class="text-caption font-weight-bold letter-spacing-1"
              :class="isOnline ? 'text-secondary' : 'text-error'"
            >
              SYSTEM {{ isOnline ? "OPERATIONAL" : "OFFLINE" }}
            </span>
          </div>
          <v-divider vertical class="mx-4 my-2" style="opacity: 0.1" />
          <div
            class="d-flex align-center text-caption text-muted font-weight-medium"
          >
            <v-icon size="14" class="mr-1">mdi-clock-outline</v-icon>
            UPTIME:
            {{
              overview?.uptime_ms
                ? formatUptime(overview.uptime_ms)
                : "--:--:--"
            }}
          </div>
        </div>
      </div>
      <v-spacer />
      <div class="actions-group d-flex align-center">
        <div class="auto-sync-badge mr-4 d-none d-sm-flex align-center">
          <div class="pulse-ring mr-2"></div>
          <span class="text-caption text-muted font-weight-bold"
            >LIVE SYNC</span
          >
        </div>
        <v-btn
          variant="tonal"
          color="primary"
          class="refresh-btn px-6 font-weight-black"
          rounded="lg"
          :loading="loading"
          @click="refresh"
          prepend-icon="mdi-refresh"
        >
          REFRESH
        </v-btn>
      </div>
    </header>

    <!-- Essential Metrics -->
    <v-row class="mb-10">
      <v-col
        v-for="card in metricCards"
        :key="card.label"
        cols="12"
        sm="6"
        md="4"
        lg="2"
      >
        <div class="glass-card metric-card h-100 pa-5">
          <div
            class="metric-icon-box mb-4"
            :style="{ '--accent-color': card.color }"
          >
            <v-icon :color="card.color" size="24">{{ card.icon }}</v-icon>
            <div class="icon-glow"></div>
          </div>
          <div class="metric-content">
            <div class="metric-label mb-1">{{ card.label }}</div>
            <div class="metric-value font-weight-black">
              {{ formatNumber(card.value) }}
            </div>
            <div class="metric-footer d-flex align-center mt-2">
              <span
                class="text-caption font-weight-bold"
                :style="{ color: card.color }"
                >{{ card.trend }}</span
              >
              <v-spacer />
              <v-icon size="16" color="muted" style="opacity: 0.3"
                >mdi-chevron-right</v-icon
              >
            </div>
          </div>
        </div>
      </v-col>
    </v-row>

    <!-- Analytics & System -->
    <v-row class="mb-10">
      <v-col cols="12" lg="8">
        <div class="glass-card chart-container h-100 pa-6">
          <div class="d-flex align-center justify-space-between mb-8">
            <div class="d-flex align-center">
              <div class="accent-line mr-3"></div>
              <h3 class="text-h6 font-weight-bold">Traffic Analyzers</h3>
            </div>
            <div class="chart-legends d-flex align-center">
              <div class="legend-item mr-6">
                <span
                  class="legend-dot"
                  style="background: var(--primary)"
                ></span>
                <span class="text-caption font-weight-bold text-muted"
                  >PUBLISHED</span
                >
              </div>
              <div class="legend-item">
                <span
                  class="legend-dot"
                  style="background: var(--secondary)"
                ></span>
                <span class="text-caption font-weight-bold text-muted"
                  >CONSUMED</span
                >
              </div>
            </div>
          </div>
          <div class="chart-wrapper">
            <v-chart class="chart" :option="throughputOption" autoresize />
          </div>
        </div>
      </v-col>

      <v-col cols="12" lg="4">
        <div class="d-flex flex-column gap-6 h-100">
          <!-- Resource Pulse -->
          <div class="glass-card system-card pa-6">
            <div class="d-flex align-center mb-6">
              <v-icon color="amber" size="20" class="mr-3">mdi-chip</v-icon>
              <h3 class="text-subtitle-1 font-weight-bold">System Pulse</h3>
            </div>

            <div class="resource-meters">
              <div class="meter-item mb-5">
                <div class="d-flex align-center justify-space-between mb-2">
                  <span class="text-caption font-weight-bold text-muted"
                    >GOROUTINES</span
                  >
                  <span class="text-body-2 font-weight-black text-amber">{{
                    overview?.system?.goroutines || 0
                  }}</span>
                </div>
                <v-progress-linear
                  model-value="65"
                  color="amber"
                  height="4"
                  rounded
                />
              </div>

              <div class="meter-item">
                <div class="d-flex align-center justify-space-between mb-2">
                  <span class="text-caption font-weight-bold text-muted"
                    >HEAP MEMORY</span
                  >
                  <span class="text-body-2 font-weight-black text-amber">{{
                    formatBytes(overview?.system?.memory || 0)
                  }}</span>
                </div>
                <v-progress-linear
                  model-value="45"
                  color="amber"
                  height="4"
                  rounded
                />
              </div>
            </div>
          </div>

          <!-- Storage Integrity -->
          <div class="glass-card system-card pa-6 flex-grow-1">
            <div class="d-flex align-center mb-6">
              <v-icon color="accent" size="20" class="mr-3"
                >mdi-database</v-icon
              >
              <h3 class="text-subtitle-1 font-weight-bold">
                Storage Integrity
              </h3>
            </div>

            <div
              class="storage-info text-center py-6 bg-surface-subtle rounded-lg mb-4"
            >
              <div class="text-caption font-weight-bold text-muted mb-1">
                CURRENT DATA VOLUME
              </div>
              <div class="text-h4 font-weight-black text-accent-light">
                {{ formatBytes(overview?.storage?.size || 0) }}
              </div>
            </div>

            <div class="storage-details">
              <v-progress-linear
                :model-value="storagePercent"
                color="accent"
                height="6"
                rounded
                class="mb-3 progress-glow"
              />
              <div
                class="d-flex justify-space-between text-caption font-weight-bold text-muted"
              >
                <span
                  >MODE:
                  {{ overview?.storage?.mode?.toUpperCase() || "N/A" }}</span
                >
                <span
                  >THRESHOLD:
                  {{
                    formatBytes(overview?.storage?.compaction_threshold || 0)
                  }}</span
                >
              </div>
            </div>
          </div>
        </div>
      </v-col>
    </v-row>

    <!-- Data Explorers -->
    <v-row>
      <v-col cols="12" md="6">
        <div class="glass-card table-glass pa-0">
          <div class="pa-6 d-flex align-center">
            <v-icon color="primary" class="mr-3">mdi-tray-full</v-icon>
            <h3 class="text-h6 font-weight-bold">Active Message Hubs</h3>
            <v-spacer />
            <v-btn
              to="/queues"
              variant="text"
              color="primary"
              size="small"
              rounded="lg"
              >EXPLORE ALL</v-btn
            >
          </div>
          <v-table class="premium-table">
            <thead>
              <tr>
                <th class="text-left">IDENTIFIER</th>
                <th class="text-right">VOLUME</th>
                <th class="text-right">FAILURE RATE</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="q in queueRows.slice(0, 5)" :key="q.name">
                <td class="mono font-weight-bold text-primary">{{ q.name }}</td>
                <td class="text-right mono font-weight-black">
                  {{ formatNumber(q.messages) }}
                </td>
                <td class="text-right">
                  <v-chip
                    v-if="q.deadLetters > 0"
                    size="x-small"
                    color="error"
                    variant="flat"
                    class="font-weight-black"
                  >
                    {{ q.deadLetters }} DLQ
                  </v-chip>
                  <span v-else class="text-muted opacity-30">—</span>
                </td>
              </tr>
            </tbody>
          </v-table>
        </div>
      </v-col>

      <v-col cols="12" md="6">
        <div class="glass-card table-glass pa-0">
          <div class="pa-6 d-flex align-center">
            <v-icon color="secondary" class="mr-3">mdi-broadcast</v-icon>
            <h3 class="text-h6 font-weight-bold">Global Topics</h3>
            <v-spacer />
            <v-btn
              to="/topics"
              variant="text"
              color="secondary"
              size="small"
              rounded="lg"
              >EXPLORE ALL</v-btn
            >
          </div>
          <v-table class="premium-table">
            <thead>
              <tr>
                <th class="text-left">NAMESPACE</th>
                <th class="text-right">PROPAGATION</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="t in topicRows.slice(0, 5)" :key="t.name">
                <td class="mono font-weight-bold text-secondary">
                  {{ t.name }}
                </td>
                <td class="text-right">
                  <div
                    class="subscriber-badge d-inline-flex align-center px-3 py-1 rounded-pill"
                  >
                    <v-icon size="12" color="secondary" class="mr-2"
                      >mdi-account-group</v-icon
                    >
                    <span class="text-caption font-weight-black text-secondary"
                      >{{ t.subscribers }} ENTHUSIASTS</span
                    >
                  </div>
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
const MAX_HISTORY = 30;

const metricCards = computed(() => {
  const m = overview.value?.metrics || {};
  return [
    {
      label: "TOTAL PUBLISHED",
      value: m.messages_published || 0,
      icon: "mdi-cloud-upload",
      color: "#00f2ff",
      trend: "LIVE STREAM",
    },
    {
      label: "TOTAL CONSUMED",
      value: m.messages_consumed || 0,
      icon: "mdi-cloud-download",
      color: "#00ff88",
      trend: "REAL TIME",
    },
    {
      label: "PENDING SYNC",
      value: overview.value?.stats?.PendingCount || 0,
      icon: "mdi-vibrate",
      color: "#ffc107",
      trend: "PROCESSING",
    },
    {
      label: "ACKNOWLEDGED",
      value: m.messages_acked || 0,
      icon: "mdi-shield-check-outline",
      color: "#4caf50",
      trend: "SECURED",
    },
    {
      label: "FAILED / NACK",
      value: m.messages_nacked || 0,
      icon: "mdi-alert-octagon-outline",
      color: "#ff5252",
      trend: "RETRYING",
    },
    {
      label: "DEAD LETTERS",
      value: m.dead_letter_count || 0,
      icon: "mdi-skull-scan-outline",
      color: "#ff1744",
      trend: "CRITICAL",
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
      left: "0%",
      right: "2%",
      bottom: "0%",
      top: "10%",
      containLabel: true,
    },
    tooltip: {
      trigger: "axis",
      backgroundColor: "rgba(9, 9, 11, 0.95)",
      borderColor: "rgba(255, 255, 255, 0.15)",
      borderWidth: 1,
      padding: [12, 16],
      textStyle: {
        color: "#fff",
        fontFamily: "Inter",
        fontSize: 12,
      },
      axisPointer: {
        lineStyle: {
          color: "rgba(255, 255, 255, 0.2)",
          type: "dashed",
        },
      },
    },
    xAxis: {
      type: "category",
      data: history.value.map((h) => h.time),
      axisLine: { show: false },
      axisTick: { show: false },
      axisLabel: { color: "#626771", fontSize: 10, margin: 15 },
    },
    yAxis: {
      type: "value",
      splitLine: {
        lineStyle: {
          color: "rgba(255, 255, 255, 0.03)",
          type: "solid",
        },
      },
      axisLabel: { color: "#626771", fontSize: 10 },
    },
    series: [
      {
        name: "PUBLISHED",
        type: "line",
        smooth: 0.4,
        showSymbol: false,
        data: history.value.map((h) => h.published),
        lineStyle: {
          width: 4,
          color: "#00f2ff",
          shadowBlur: 20,
          shadowColor: "rgba(0, 242, 155, 0.5)",
        },
        areaStyle: {
          opacity: 0.05,
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
        name: "CONSUMED",
        type: "line",
        smooth: 0.4,
        showSymbol: false,
        data: history.value.map((h) => h.consumed),
        lineStyle: {
          width: 4,
          color: "#00ff88",
          shadowBlur: 20,
          shadowColor: "rgba(0, 255, 136, 0.5)",
        },
        areaStyle: {
          opacity: 0.05,
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
  if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + "M";
  if (n >= 1_000) return (n / 1_000).toFixed(1) + "K";
  return n.toLocaleString();
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + " " + sizes[i];
}

function formatUptime(ms: number): string {
  const seconds = Math.floor(ms / 1000);
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = seconds % 60;
  return `${h.toString().padStart(2, "0")}:${m.toString().padStart(2, "0")}:${s.toString().padStart(2, "0")}`;
}

async function refresh() {
  loading.value = true;
  try {
    const data = await api.getOverview();
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

useIntervalFn(refresh, 3000);
onMounted(refresh);
</script>

<style lang="scss" scoped>
.premium-dashboard {
  color: var(--text-primary);
}

.bg-surface-subtle {
  background: rgba(255, 255, 255, 0.03);
  border: 1px solid var(--glass-border);
}

.letter-spacing-1 {
  letter-spacing: 0.1em;
}

.status-indicator-group {
  box-shadow: 0 0 20px rgba(0, 0, 0, 0.2);
}

.chart-wrapper {
  height: 350px;
}

.metric-card {
  position: relative;
  overflow: hidden;

  .metric-icon-box {
    width: 48px;
    height: 48px;
    background: rgba(var(--accent-color), 0.1);
    border-radius: 12px;
    display: flex;
    align-items: center;
    justify-content: center;
    position: relative;

    .icon-glow {
      position: absolute;
      width: 100%;
      height: 100%;
      background: var(--accent-color);
      filter: blur(20px);
      opacity: 0.05;
      z-index: 0;
    }

    .v-icon {
      z-index: 1;
    }
  }
}

.accent-line {
  width: 4px;
  height: 24px;
  background: var(--primary);
  border-radius: 4px;
  box-shadow: 0 0 10px var(--primary);
}

.legend-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  display: inline-block;
  margin-right: 8px;
}

.pulse-ring {
  width: 8px;
  height: 8px;
  background: var(--secondary);
  border-radius: 50%;
  position: relative;

  &::before {
    content: "";
    position: absolute;
    width: 100%;
    height: 100%;
    background: var(--secondary);
    border-radius: 50%;
    animation: pulse 2s infinite;
  }
}

.progress-glow {
  box-shadow: 0 0 15px rgba(191, 0, 255, 0.2);
}

.text-accent-light {
  color: #ea9fff;
  text-shadow: 0 0 20px rgba(191, 0, 255, 0.3);
}

.subscriber-badge {
  background: rgba(0, 255, 136, 0.05);
  border: 1px solid rgba(0, 255, 136, 0.1);
}

.gap-6 {
  gap: 24px;
}

.opacity-30 {
  opacity: 0.3;
}
</style>
