import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
    vus: 10,
    duration: '5s',
};

const BASE_URL = __ENV.BOLTQ_ADDR || 'http://localhost:9090';

export default function () {
    let targetTopic = 'persistence_test';
    const isDLQ = Math.random() > 0.95;
    const isDelayed = Math.random() > 0.8;

    if (isDLQ) {
        targetTopic = 'persistence_test_dead_letter';
    }
    
    let delay = 0;
    
    if (isDelayed) {
        delay = 60 * 1000 * 1000 * 1000; // 60 seconds in nanoseconds
    }

    const payload = JSON.stringify({
        topic: targetTopic,
        payload: {
            msg: `test message ${Math.random()}`,
            time: new Date().toISOString()
        },
        delay: delay
    });

    const params = {
        headers: {
            'Content-Type': 'application/json',
        },
    };

    const res = http.post(`${BASE_URL}/publish`, payload, params);
    check(res, {
        'status is 200': (r) => r.status === 200,
        'status is published': (r) => r.json().status === 'published',
    });
}
