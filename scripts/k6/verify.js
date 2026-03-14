import http from 'k6/http';
import { check } from 'k6';

export const options = {
    vus: 1,
    iterations: 1,
};

const BASE_URL = __ENV.BOLTQ_ADDR || 'http://localhost:9090';

export default function () {
    const res = http.get(`${BASE_URL}/overview`);
    check(res, {
        'status is 200': (r) => r.status === 200,
    });

    const overview = res.json();
    const stats = overview.stats;
    const count = stats.Queues['persistence_test'] || 0;
    const dlqCount = stats.DeadLetters['persistence_test_dead_letter'] || 0;
    const delayedCount = stats.DelayedCount || 0;
    
    console.log(`Queue depth for 'persistence_test': ${count}`);
    console.log(`DLQ depth for 'persistence_test_dead_letter': ${dlqCount}`);
    console.log(`Delayed messages count: ${delayedCount}`);

    if (count === 0 && delayedCount === 0 && dlqCount === 0) {
        console.error("FAILED: No messages found in persistence!");
    } else {
        console.log(`SUCCESS: Found ${count} active, ${dlqCount} DLQ, and ${delayedCount} delayed messages persisted.`);
    }
}
