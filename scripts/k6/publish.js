import http from "k6/http";
import { check, sleep } from "k6";

export const options = {
  scenarios: {
    normal_publish: {
      executor: "shared-iterations",
      vus: 10,
      iterations: 10000,
      startTime: "0s",
      exec: "publishNormal",
    },
    dlq_publish: {
      executor: "shared-iterations",
      vus: 5,
      iterations: 5000,
      startTime: "0s",
      exec: "publishDLQ",
    },
  },
};

const BASE_URL = __ENV.BOLTQ_ADDR || "http://localhost:9090";

function publish(topic) {
  const isDelayed = Math.random() > 0.8;
  let delay = 0;

  if (isDelayed) {
    delay = 60 * 1000 * 1000 * 1000; // 60 seconds in nanoseconds
  }

  const payload = JSON.stringify({
    topic: topic,
    payload: {
      msg: `test message ${Math.random()}`,
      time: new Date().toISOString(),
    },
    delay: delay,
  });

  const params = {
    headers: {
      "Content-Type": "application/json",
    },
  };

  const res = http.post(`${BASE_URL}/publish`, payload, params);
  check(res, {
    "status is 200": (r) => r.status === 200,
    "status is published": (r) => r.json().status === "published",
  });
}

export function publishNormal() {
  publish("persistence_test");
}

export function publishDLQ() {
  publish("persistence_test_dead_letter");
}
