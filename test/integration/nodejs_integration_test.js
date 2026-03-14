import boltq from '../../client/nodejs/index.js';
import assert from 'assert';
import { setTimeout } from 'timers/promises';

const addr = '127.0.0.1';
const port = 9191; // Test port

async function runTests() {
  const client = new boltq(addr, port);
  
  try {
    console.log('Connecting to BoltQ...');
    await client.connect();
    console.log('Connected.');

    // 1. Delayed and TTL test
    console.log('\n--- Testing Delayed and TTL ---');
    const topic = 'node_delayed_test';
    const { id } = await client.publish(topic, { hello: 'node' }, null, { delay: 1000 });
    console.log(`Published delayed message: ${id}`);

    let msg = await client.consume(topic);
    assert.strictEqual(msg, null, 'Message should be hidden during delay');
    console.log('Confirmed: Message hidden during delay.');

    console.log('Waiting for delay...');
    await setTimeout(1500);

    msg = await client.consume(topic);
    assert.ok(msg, 'Message should be available after delay');
    assert.strictEqual(msg.id, id);
    console.log('Confirmed: Message received after delay.');
    await client.ack(msg.id);

    // 2. Prefetch test
    console.log('\n--- Testing Prefetch ---');
    await client.setPrefetch(1);
    console.log('Prefetch set to 1.');

    await client.publish('prefetch_test', 'msg1');
    await client.publish('prefetch_test', 'msg2');

    const m1 = await client.consume('prefetch_test');
    assert.ok(m1, 'Should consume m1');
    console.log('Consumed m1.');

    try {
      await client.consume('prefetch_test');
      assert.fail('Should have thrown prefetch limit error');
    } catch (err) {
      assert.strictEqual(err.code, 'SERVER_ERROR');
      assert.ok(err.message.includes('prefetch limit'), 'Error should mention prefetch limit');
      console.log('Confirmed: Prefetch limit reached.');
    }

    await client.ack(m1.id);
    console.log('Acked m1.');

    const m2 = await client.consume('prefetch_test');
    assert.ok(m2, 'Should consume m2 after m1 ack');
    console.log('Consumed m2.');
    await client.ack(m2.id);

    // 3. Durable Subscribe test
    console.log('\n--- Testing Durable Subscribe ---');
    const pubsubTopic = 'node_pubsub_test';
    let received = null;
    
    await client.subscribe(pubsubTopic, 'node_sub1', { durable: true }, (msg) => {
      console.log(`Subscription received message: ${JSON.stringify(msg.payload)}`);
      received = msg;
    });
    console.log('Subscribed to topic.');

    await client.publishTopic(pubsubTopic, 'hello pubsub');
    console.log('Published to topic.');

    // Wait for message delivery via background streaming
    let waitCount = 0;
    while (!received && waitCount < 20) {
      await setTimeout(200);
      waitCount++;
    }

    assert.ok(received, 'Should have received pubsub message');
    console.log('Confirmed: PubSub message received.');

    console.log('\nAll tests passed successfully!');
    process.exit(0);
  } catch (err) {
    console.error('\nTest failed:', err);
    process.exit(1);
  } finally {
    client.disconnect();
  }
}

runTests();
