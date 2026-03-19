/**
 * BoltQ Node.js Client SDK
 * TCP protocol client for the BoltQ message queue server.
 * Uses native net.Socket for persistent TCP connections.
 */

import { Socket } from 'net';
import { EventEmitter } from 'events';

// Command bytes (must match server protocol)
const CMD_PUBLISH       = 0x01;
const CMD_PUBLISH_TOPIC = 0x02;
const CMD_CONSUME       = 0x03;
const CMD_ACK           = 0x04;
const CMD_NACK          = 0x05;
const CMD_PING          = 0x06;
const CMD_STATS         = 0x07;
const CMD_AUTH          = 0x08;
const CMD_PREFETCH      = 0x09;
const CMD_CLUSTER_JOIN   = 0x10;
const CMD_CLUSTER_LEAVE  = 0x11;
const CMD_CLUSTER_STATUS = 0x12;
const CMD_CONFIRM_SELECT   = 0x13;
const CMD_EXCHANGE_DECLARE = 0x14;
const CMD_EXCHANGE_DELETE  = 0x15;
const CMD_BIND_QUEUE       = 0x16;
const CMD_UNBIND_QUEUE     = 0x17;
const CMD_PUBLISH_EXCHANGE = 0x18;

// Response status bytes
const STATUS_OK         = 0x00;
const STATUS_ERROR      = 0x01;
const STATUS_EMPTY      = 0x02;
const STATUS_NOT_LEADER = 0x03;

// Frame header: 1 byte command + 4 bytes LE length = 5 bytes
const HEADER_SIZE = 5;

export class BoltQError extends Error {
  constructor(message, code, details = {}) {
    super(message);
    this.name = 'BoltQError';
    this.code = code || 'BOLTQ_ERROR';
    this.leader = details.leader || null;
    this.leaderId = details.leader_id || null;
    this.details = details;
  }
}

export class BoltQClient extends EventEmitter {
  /**
   * @param {string} host - Server hostname (e.g. "localhost")
   * @param {number} port - Server TCP port (e.g. 9091)
   * @param {object} [options]
   * @param {string} [options.apiKey] - API key for authentication
   * @param {number} [options.timeout] - Request timeout in milliseconds (default: 30000)
   */
  constructor(host, port, options = {}) {
    super();
    this.host = host;
    this.port = port;
    this.apiKey = options.apiKey || null;
    this.timeout = options.timeout ?? 30000;
    this.autoReconnect = options.autoReconnect !== false;
    this.reconnectInterval = options.reconnectInterval ?? 2000;

    /** @type {Socket|null} */
    this._socket = null;
    this._connected = false;
    this._authenticated = false;
    this._closing = false;

    // Track subscriptions for recovery
    // Map: topic:subscriberId -> { options, onMessage, handler }
    this._subscriptions = new Map();

    // Request queue for serializing commands over a single connection
    this._pending = []; // [{resolve, reject, timer}]
    this._buffer = Buffer.alloc(0);
  }

  /**
   * Connect to the server and authenticate if apiKey is set.
   * @returns {Promise<void>}
   */
  async connect() {
    if (this._connected) return;

    await new Promise((resolve, reject) => {
      this._socket = new Socket();

      const onError = (err) => {
        this._socket.removeListener('connect', onConnect);
        reject(new BoltQError(`Connection failed: ${err.message}`, 'CONNECT_ERROR'));
      };

      const onConnect = () => {
        this._socket.removeListener('error', onError);
        this._connected = true;

        this._socket.on('data', (chunk) => this._onData(chunk));
        this._socket.on('error', (err) => this._onError(err));
        this._socket.on('close', () => this._onClose());

        resolve();
      };

      this._socket.once('error', onError);
      this._socket.once('connect', onConnect);
      this._socket.connect(this.port, this.host);
    });

    // Authenticate if API key is set
    if (this.apiKey) {
      const res = await this._sendCommand(CMD_AUTH, { api_key: this.apiKey });
      if (res.status !== STATUS_OK) {
        this.disconnect();
        const payload = res.payload ? JSON.parse(res.payload) : {};
        throw new BoltQError(payload.error || 'Authentication failed', 'AUTH_ERROR');
      }
    }
    this._authenticated = true;
  }

  /**
   * Disconnect from the server.
   */
  disconnect() {
    this._closing = true;
    if (this._socket) {
      this._socket.destroy();
      this._socket = null;
    }
    this._connected = false;
    this._authenticated = false;

    // Reject all pending requests
    for (const p of this._pending) {
      clearTimeout(p.timer);
      p.reject(new BoltQError('Connection closed', 'CLOSED'));
    }
    this._pending = [];
    this._buffer = Buffer.alloc(0);
  }

  // ---------------------------------------------------------------------------
  // Public API
  // ---------------------------------------------------------------------------

  /**
   * Publish a message to a queue topic.
   * @param {string} topic
   * @param {any} payload
   * @param {Record<string, string>} [headers]
   * @param {object} [options]
   * @param {number} [options.delay] - Delay in milliseconds
   * @param {number} [options.ttl] - TTL in milliseconds
   * @returns {Promise<{id: string, topic: string}>}
   */
  async publish(topic, payload, headers, options = {}) {
    return this._command(CMD_PUBLISH, {
      topic,
      payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
      headers: headers || {},
      delay: options.delay || 0,
      ttl: options.ttl || 0,
      priority: options.priority || 0,
    });
  }

  /**
   * Publish a message to a pub/sub topic.
   * @param {string} topic
   * @param {any} payload
   * @param {Record<string, string>} [headers]
   * @param {object} [options]
   * @param {number} [options.delay] - Delay in milliseconds
   * @param {number} [options.ttl] - TTL in milliseconds
   * @returns {Promise<{id: string, topic: string}>}
   */
  async publishTopic(topic, payload, headers, options = {}) {
    return this._command(CMD_PUBLISH_TOPIC, {
      topic,
      payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
      headers: headers || {},
      delay: options.delay || 0,
      ttl: options.ttl || 0,
      priority: options.priority || 0,
    });
  }

  /**
   * Consume a single message from a queue topic.
   * Returns null when no messages are available.
   * @param {string} topic
   * @returns {Promise<object|null>}
   */
  async consume(topic) {
    const res = await this._sendCommand(CMD_CONSUME, { topic });
    if (res.status === STATUS_EMPTY) return null;
    if (res.status === STATUS_ERROR) {
      let message = 'Consume failed';
      let code = 'SERVER_ERROR';
      try {
        const payload = JSON.parse(res.payload.toString());
        message = payload.error || message;
        code = payload.code || code;
      } catch { /* ignore */ }
      throw new BoltQError(message, code);
    }
    return JSON.parse(res.payload.toString());
  }

  /**
   * Acknowledge a consumed message.
   * @param {string} messageId
   * @returns {Promise<void>}
   */
  async ack(messageId) {
    await this._command(CMD_ACK, { id: messageId });
  }

  /**
   * Negatively acknowledge a message for redelivery.
   * @param {string} messageId
   * @returns {Promise<void>}
   */
  async nack(messageId) {
    await this._command(CMD_NACK, { id: messageId });
  }

  /**
   * Ping the server.
   * @returns {Promise<void>}
   */
  async ping() {
    await this._command(CMD_PING, {});
  }

  /**
   * Get broker statistics.
   * @returns {Promise<object>}
   */
  async stats() {
    return this._command(CMD_STATS, {});
  }

  /**
   * Set prefetch limit for this client.
   * @param {number} count
   * @returns {Promise<void>}
   */
  async setPrefetch(count) {
    await this._command(CMD_PREFETCH, { count });
  }

  /**
   * Subscribe to a pub/sub topic.
   * @param {string} topic
   * @param {string} subscriberId
   * @param {object} [options]
   * @param {boolean} [options.durable]
   * @param {(msg: any) => void} [onMessage] - Callback for incoming messages
   * @returns {Promise<void>}
   */
  async subscribe(topic, subscriberId, options = {}, onMessage = null) {
    const res = await this._sendCommand(CMD_CONSUME, {
      topic,
      id: subscriberId,
      durable: !!options.durable
    });

    if (res.status !== STATUS_OK) {
      const payload = res.payload.length > 0 ? JSON.parse(res.payload) : {};
      throw new BoltQError(payload.error || 'Subscribe failed', 'SUBSCRIBE_ERROR');
    }

    const key = `${topic}:${subscriberId}`;
    if (onMessage) {
      const wrappedHandler = (msg) => {
        if (msg.topic === topic && msg.subscriber_id === subscriberId) {
          onMessage(msg);
        }
      };
      this._subscriptions.set(key, { topic, subscriberId, options, onMessage: wrappedHandler });
      this.on('message', wrappedHandler);
    } else {
      this._subscriptions.set(key, { topic, subscriberId, options });
    }
  }

  /**
   * Health check. Returns true if ping succeeds.
   * @returns {Promise<boolean>}
   */
  async health() {
    try {
      await this.ping();
      return true;
    } catch {
      return false;
    }
  }

  /**
   * Start polling a queue topic.
   * @param {string} topic
   * @param {(msg: object) => Promise<void>} handler
   * @param {number} [intervalMs=1000]
   * @returns {{ stop: () => void }}
   */
  startConsumer(topic, handler, intervalMs = 1000) {
    let running = true;

    const poll = async () => {
      while (running) {
        try {
          const msg = await this.consume(topic);
          if (msg) {
            try {
              await handler(msg);
              await this.ack(msg.id);
            } catch (err) {
              console.error(`[boltq] handler error for ${msg.id}: ${err}`);
              try { await this.nack(msg.id); } catch { /* ignore */ }
            }
          } else {
            await new Promise((r) => setTimeout(r, intervalMs));
          }
        } catch (err) {
          if (!running) break;
          console.error(`[boltq] poll error on "${topic}": ${err}`);
          await new Promise((r) => setTimeout(r, intervalMs * 2));
        }
      }
    };

    poll();
    return { stop: () => { running = false; } };
  }

  // ---------------------------------------------------------------------------
  // Publisher Confirm & Exchange Routing
  // ---------------------------------------------------------------------------

  /**
   * Enable publisher confirm mode.
   * @returns {Promise<void>}
   */
  async enableConfirm() {
    await this._command(CMD_CONFIRM_SELECT, {});
  }

  /**
   * Declare an exchange.
   * @param {string} name
   * @param {'direct'|'fanout'|'topic'|'headers'} [type='direct']
   * @param {boolean} [durable=false]
   * @returns {Promise<void>}
   */
  async exchangeDeclare(name, type = 'direct', durable = false) {
    await this._command(CMD_EXCHANGE_DECLARE, { name, type, durable });
  }

  /**
   * Delete an exchange.
   * @param {string} name
   * @returns {Promise<void>}
   */
  async exchangeDelete(name) {
    await this._command(CMD_EXCHANGE_DELETE, { name });
  }

  /**
   * Bind a queue to an exchange.
   * @param {string} exchange
   * @param {string} queue
   * @param {string} [bindingKey='']
   * @param {object} [options]
   * @param {Record<string,string>} [options.headers]
   * @param {boolean} [options.matchAll]
   * @returns {Promise<void>}
   */
  async bindQueue(exchange, queue, bindingKey = '', options = {}) {
    await this._command(CMD_BIND_QUEUE, {
      exchange, queue, binding_key: bindingKey,
      headers: options.headers || null,
      match_all: options.matchAll || false,
    });
  }

  /**
   * Unbind a queue from an exchange.
   * @param {string} exchange
   * @param {string} queue
   * @param {string} [bindingKey='']
   * @returns {Promise<void>}
   */
  async unbindQueue(exchange, queue, bindingKey = '') {
    await this._command(CMD_UNBIND_QUEUE, { exchange, queue, binding_key: bindingKey });
  }

  /**
   * Publish a message to an exchange with a routing key.
   * @param {string} exchange
   * @param {string} routingKey
   * @param {any} payload
   * @param {Record<string,string>} [headers]
   * @param {object} [options]
   * @param {number} [options.priority] - Priority 0-9
   * @returns {Promise<{id: string}>}
   */
  async publishToExchange(exchange, routingKey, payload, headers, options = {}) {
    return this._command(CMD_PUBLISH_EXCHANGE, {
      exchange,
      routing_key: routingKey,
      payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
      headers: headers || {},
      priority: options.priority || 0,
    });
  }

  // ---------------------------------------------------------------------------
  // Internal
  // ---------------------------------------------------------------------------

  /**
   * Send a command and parse the response, throwing on error.
   */
  async _command(cmd, data) {
    const res = await this._sendCommand(cmd, data);
    if (res.status !== STATUS_OK) {
      if (res.status === STATUS_EMPTY) return null;

      let message = 'Server error';
      let code = res.status === STATUS_NOT_LEADER ? 'NOT_LEADER' : 'SERVER_ERROR';
      let details = {};
      try {
        details = JSON.parse(res.payload.toString());
        message = details.error || message;
      } catch { /* ignore */ }
      throw new BoltQError(message, code, details);
    }
    if (res.payload.length === 0) return null;
    return JSON.parse(res.payload.toString());
  }

  /**
   * Send a raw command frame and wait for the response frame.
   * @returns {Promise<{status: number, payload: Buffer}>}
   */
  _sendCommand(cmd, data) {
    if (!this._connected) {
      return Promise.reject(new BoltQError('Not connected', 'NOT_CONNECTED'));
    }

    return new Promise((resolve, reject) => {
      const payload = Buffer.from(JSON.stringify(data), 'utf-8');
      const frame = Buffer.alloc(HEADER_SIZE + payload.length);
      frame[0] = cmd;
      frame.writeUInt32LE(payload.length, 1);
      payload.copy(frame, HEADER_SIZE);

      const timer = setTimeout(() => {
        const idx = this._pending.findIndex((p) => p.resolve === resolve);
        if (idx !== -1) this._pending.splice(idx, 1);
        reject(new BoltQError('Request timed out', 'TIMEOUT'));
      }, this.timeout);

      this._pending.push({ resolve, reject, cmd, timer });
      this._socket.write(frame);
    });
  }

  /**
   * Handle incoming data, buffering and extracting complete frames.
   */
  _onData(chunk) {
    this._buffer = Buffer.concat([this._buffer, chunk]);
    this._drain();
  }

  /**
   * Try to extract complete frames from the buffer.
   */
  _drain() {
    while (this._buffer.length >= HEADER_SIZE) {
      const length = this._buffer.readUInt32LE(1);
      const totalSize = HEADER_SIZE + length;
      if (this._buffer.length < totalSize) break;

      const status = this._buffer[0];
      const payloadRaw = this._buffer.subarray(HEADER_SIZE, totalSize);
      this._buffer = this._buffer.subarray(totalSize);

      // Check if this is a streamed message (STATUS_OK with subscriber_id)
      if (status === STATUS_OK) {
        try {
          const payloadStr = payloadRaw.toString();
          if (payloadStr.includes('"subscriber_id"')) {
            const msg = JSON.parse(payloadStr);
            if (msg.topic && msg.subscriber_id) {
              this.emit('message', msg);
              this.emit(`message:${msg.topic}`, msg);
              this.emit(`message:${msg.topic}:${msg.subscriber_id}`, msg);
              continue;
            }
          }
        } catch { /* ignore parsing errors for streaming check */ }
      }

      // Handle responses to pending requests
      if (this._pending.length > 0) {
        const pending = this._pending[0];
        clearTimeout(pending.timer);
        this._pending.shift();
        pending.resolve({ status, payload: payloadRaw });
        continue;
      }

      // If no pending request and not a streamed message, log it
      console.warn('[boltq] received unexpected frame:', status, payloadRaw.toString());
    }
  }

  _onError(err) {
    this.emit('error', err);
  }

  _onClose() {
    const wasConnected = this._connected;
    this._connected = false;
    this._authenticated = false;
    this.emit('close');

    // Reject remaining pending requests
    for (const p of this._pending) {
      clearTimeout(p.timer);
      p.reject(new BoltQError('Connection closed', 'CLOSED'));
    }
    this._pending = [];
    this._buffer = Buffer.alloc(0);

    if (this.autoReconnect && !this._closing && wasConnected) {
      setTimeout(() => {
        this.connect()
          .then(() => {
            console.log('[boltq] reconnected, recovering subscriptions...');
            for (const sub of this._subscriptions.values()) {
              this._sendCommand(CMD_CONSUME, {
                topic: sub.topic,
                id: sub.subscriberId,
                durable: !!sub.options.durable
              }).catch(err => {
                console.error(`[boltq] failed to recover subscription to ${sub.topic}:`, err.message);
              });
            }
          })
          .catch(err => {
            console.error('[boltq] reconnection failed:', err.message);
            this._onClose(); // Retry again
          });
      }, this.reconnectInterval);
    }
  }
}

  // ---------------------------------------------------------------------------
  // Cache / KV Store (HTTP-based)
  // ---------------------------------------------------------------------------

  /**
   * Returns the HTTP admin base URL derived from TCP host/port.
   * Convention: HTTP port = TCP port - 1 (e.g., TCP 9091 -> HTTP 9090).
   * @returns {string}
   */
  get _httpBase() {
    return `http://${this.host}:${this.port - 1}`;
  }

  /**
   * Perform an HTTP request to the admin API.
   * @param {string} method
   * @param {string} path
   * @param {object} [body]
   * @returns {Promise<any>}
   */
  async _httpRequest(method, path, body) {
    const url = `${this._httpBase}${path}`;
    const headers = { 'Content-Type': 'application/json' };
    if (this.apiKey) headers['X-API-Key'] = this.apiKey;

    const opts = { method, headers };
    if (body && method !== 'GET') {
      opts.body = JSON.stringify(body);
    }

    const res = await fetch(url, opts);
    const data = await res.json();
    if (!res.ok) throw new BoltQError(data.error || `HTTP ${res.status}`, 'HTTP_ERROR', data);
    return data;
  }

  /**
   * Get a value from cache.
   * @param {string} key
   * @returns {Promise<{key: string, value: any, ttl: number, created_at: number}>}
   */
  async cacheGet(key) {
    return this._httpRequest('GET', `/cache/get?key=${encodeURIComponent(key)}`);
  }

  /**
   * Set a key-value pair in cache.
   * @param {string} key
   * @param {any} value
   * @param {number} [ttl=0] - TTL in milliseconds, 0 = no expiry
   * @returns {Promise<{status: string, key: string}>}
   */
  async cacheSet(key, value, ttl = 0) {
    return this._httpRequest('POST', '/cache/set', { key, value, ttl });
  }

  /**
   * Delete a key from cache.
   * @param {string} key
   * @returns {Promise<{status: string, deleted: boolean}>}
   */
  async cacheDel(key) {
    return this._httpRequest('POST', '/cache/del', { key });
  }

  /**
   * List keys matching a pattern.
   * @param {string} [pattern='*']
   * @returns {Promise<{keys: string[], count: number}>}
   */
  async cacheKeys(pattern = '*') {
    return this._httpRequest('GET', `/cache/keys?pattern=${encodeURIComponent(pattern)}`);
  }

  /**
   * Check if a key exists.
   * @param {string} key
   * @returns {Promise<{key: string, exists: boolean}>}
   */
  async cacheExists(key) {
    return this._httpRequest('GET', `/cache/exists?key=${encodeURIComponent(key)}`);
  }

  /**
   * Set TTL on an existing key (milliseconds). Use 0 to remove TTL.
   * @param {string} key
   * @param {number} ttl
   * @returns {Promise<{status: string, key: string, found: boolean}>}
   */
  async cacheExpire(key, ttl) {
    return this._httpRequest('POST', '/cache/expire', { key, ttl });
  }

  /**
   * Atomically increment a numeric value. Creates key if not exists.
   * @param {string} key
   * @param {number} [delta=1]
   * @returns {Promise<{key: string, value: number}>}
   */
  async cacheIncr(key, delta = 1) {
    return this._httpRequest('POST', '/cache/incr', { key, delta });
  }

  /**
   * Flush all cache entries.
   * @returns {Promise<{status: string, removed: number}>}
   */
  async cacheFlush() {
    return this._httpRequest('POST', '/cache/flush');
  }

  /**
   * Get cache statistics.
   * @returns {Promise<{enabled: boolean, stats: object}>}
   */
  async cacheStats() {
    return this._httpRequest('GET', '/cache/stats');
  }
}

export default BoltQClient;
