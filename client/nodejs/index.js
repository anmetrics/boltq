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

// Response status bytes
const STATUS_OK    = 0x00;
const STATUS_ERROR = 0x01;
const STATUS_EMPTY = 0x02;

// Frame header: 1 byte command + 4 bytes LE length = 5 bytes
const HEADER_SIZE = 5;

export class BoltQError extends Error {
  constructor(message, code) {
    super(message);
    this.name = 'BoltQError';
    this.code = code || 'BOLTQ_ERROR';
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

    /** @type {Socket|null} */
    this._socket = null;
    this._connected = false;
    this._authenticated = false;

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
   * @returns {Promise<{id: string, topic: string}>}
   */
  async publish(topic, payload, headers) {
    return this._command(CMD_PUBLISH, {
      topic,
      payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
      headers: headers || {},
    });
  }

  /**
   * Publish a message to a pub/sub topic.
   * @param {string} topic
   * @param {any} payload
   * @param {Record<string, string>} [headers]
   * @returns {Promise<{id: string, topic: string}>}
   */
  async publishTopic(topic, payload, headers) {
    return this._command(CMD_PUBLISH_TOPIC, {
      topic,
      payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
      headers: headers || {},
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
      const payload = res.payload.length > 0 ? JSON.parse(res.payload) : {};
      throw new BoltQError(payload.error || 'Consume failed', 'CONSUME_ERROR');
    }
    return JSON.parse(res.payload);
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
  // Internal
  // ---------------------------------------------------------------------------

  /**
   * Send a command and parse the response, throwing on error.
   */
  async _command(cmd, data) {
    const res = await this._sendCommand(cmd, data);
    if (res.status === STATUS_ERROR) {
      const payload = res.payload.length > 0 ? JSON.parse(res.payload) : {};
      throw new BoltQError(payload.error || 'Server error', 'SERVER_ERROR');
    }
    if (res.payload.length === 0) return null;
    return JSON.parse(res.payload);
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

      this._pending.push({ resolve, reject, timer });
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
      const payload = this._buffer.subarray(HEADER_SIZE, totalSize);
      this._buffer = this._buffer.subarray(totalSize);

      const pending = this._pending.shift();
      if (pending) {
        clearTimeout(pending.timer);
        pending.resolve({ status, payload });
      }
    }
  }

  _onError(err) {
    this.emit('error', err);
  }

  _onClose() {
    this._connected = false;
    this.emit('close');

    // Reject remaining pending requests
    for (const p of this._pending) {
      clearTimeout(p.timer);
      p.reject(new BoltQError('Connection closed', 'CLOSED'));
    }
    this._pending = [];
    this._buffer = Buffer.alloc(0);
  }
}

export default BoltQClient;
