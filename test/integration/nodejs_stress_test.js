import boltq from '../../client/nodejs/index.js';
import assert from 'assert';
import { setTimeout } from 'timers/promises';

const addr = '127.0.0.1';
const port = 9191;

async function runAdvancedTests() {
    console.log('🚀 Starting Advanced BoltQ Node.js Stress Tests...\n');

    const setupClient = async () => {
        const c = new boltq(addr, port);
        c.on('error', (err) => {
            // Only log if it's not a normal disconnect
            if (c._connected) console.warn(`[client] socket error: ${err.message}`);
        });
        await c.connect();
        return c;
    };

    // 1. Concurrency Test: Multiple Consumers on same queue
    console.log('--- Test 1: Concurrency (Load Balancing) ---');
    const topic = 'concurrency_test';
    const consumerCount = 5;
    const messageCount = 100;
    const clients = [];
    const consumers = [];
    const receivedIds = new Set();
    let totalReceived = 0;

    for (let i = 0; i < consumerCount; i++) {
        const c = await setupClient();
        clients.push(c);
        const handle = c.startConsumer(topic, async (msg) => {
            if (receivedIds.has(msg.id)) {
                console.error(`❌ Duplicate message received: ${msg.id}`);
            }
            receivedIds.add(msg.id);
            totalReceived++;
        }, 100);
        consumers.push(handle);
    }
    console.log(`Started ${consumerCount} parallel consumers.`);

    // Publisher
    const publisher = new boltq(addr, port);
    await publisher.connect();
    for (let i = 0; i < messageCount; i++) {
        await publisher.publish(topic, { index: i });
    }
    console.log(`Published ${messageCount} messages.`);

    // Wait for all to be consumed
    let waitTotal = 0;
    while (totalReceived < messageCount && waitTotal < 50) {
        await setTimeout(200);
        waitTotal++;
    }

    console.log(`Total received: ${totalReceived}/${messageCount}`);
    assert.strictEqual(totalReceived, messageCount, 'All messages should be consumed exactly once');
    console.log('✅ Concurrency test passed.\n');

    // Cleanup clients
    consumers.forEach(h => h.stop());
    await setTimeout(500); // Wait for loops to stop
    clients.forEach(c => c.disconnect());

    // 2. Durable Catch-up Test
    console.log('--- Test 2: Durable Catch-up (Offline Recovery) ---');
    const pubsubTopic = 'catchup_test';
    const subId = 'node-durable-tester';
    
    // First, subscribe and then disconnect to "register" the durable ID
    const sub1 = new boltq(addr, port);
    await sub1.connect();
    await sub1.subscribe(pubsubTopic, subId, { durable: true });
    console.log('Subscribed (durable) and disconnecting...');
    sub1.disconnect();

    // Publish while offline
    const pub2 = new boltq(addr, port);
    await pub2.connect();
    for (let i = 1; i <= 10; i++) {
        await pub2.publishTopic(pubsubTopic, `offline message ${i}`);
    }
    console.log('Published 10 messages while subscriber was offline.');
    pub2.disconnect();

    // Reconnect and catch up
    console.log('Connecting and re-subscribing for catch-up...');
    const sub2 = new boltq(addr, port);
    await sub2.connect();
    let catchupCount = 0;
    await sub2.subscribe(pubsubTopic, subId, { durable: true }, (msg) => {
        catchupCount++;
        if (catchupCount % 2 === 0) console.log(`  ... received ${catchupCount} catch-up messages`);
    });

    let waitCatchup = 0;
    while (catchupCount < 10 && waitCatchup < 50) {
        await setTimeout(200);
        waitCatchup++;
    }

    console.log(`Catch-up received: ${catchupCount}/10`);
    assert.strictEqual(catchupCount, 10, 'Should have caught up on all offline messages');
    console.log('✅ Durable catch-up test passed.\n');
    sub2.disconnect();

    // 3. High Volume Stress Test
    console.log('--- Test 3: High Volume (1000 messages) ---');
    const stressTopic = 'stress_test';
    const stressCount = 1000;
    const stressPub = new boltq(addr, port);
    stressPub.on('error', (err) => console.warn(`[stressPub] socket error: ${err.message}`));
    await stressPub.connect();
    
    console.log('Publishing 1000 messages in batches...');
    const startPub = Date.now();
    const batchSize = 100;
    for (let i = 0; i < stressCount; i += batchSize) {
        const batch = [];
        for (let j = 0; j < batchSize && (i + j) < stressCount; j++) {
            batch.push(stressPub.publish(stressTopic, { data: 'x'.repeat(100) }));
        }
        await Promise.all(batch);
    }
    const endPub = Date.now();
    console.log(`Published ${stressCount} messages in ${endPub - startPub}ms.`);

    let stressReceived = 0;
    const stressSub = new boltq(addr, port);
    stressSub.on('error', (err) => console.warn(`[stressSub] socket error: ${err.message}`));
    await stressSub.connect();
    while (stressReceived < stressCount) {
        const msg = await stressSub.consume(stressTopic);
        if (msg) {
            stressReceived++;
            await stressSub.ack(msg.id);
        } else {
            break;
        }
    }
    console.log(`Consumed ${stressReceived}/${stressCount} messages.`);
    assert.strictEqual(stressReceived, stressCount, 'Should consume all 1000 messages');
    console.log('✅ Stress test passed.\n');

    console.log('🎉 ALL ADVANCED TESTS PASSED!');
    process.exit(0);
}

runAdvancedTests().catch(err => {
    console.error('❌ Advanced tests failed:', err);
    process.exit(1);
});
