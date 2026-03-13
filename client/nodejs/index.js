/**
 * BoltQ Node.js Client SDK
 * Zero-dependency client for the BoltQ message queue server.
 * Requires Node.js 18+ (native fetch).
 */

export class BoltQError extends Error {
  constructor(message, statusCode, body) {
    super(message);
    this.name = 'BoltQError';
    this.statusCode = statusCode;
    this.body = body;
  }
}

export class BoltQClient {
  /**
   * @param {string} baseURL - The base URL of the BoltQ server (e.g. "http://localhost:9090")
   * @param {object} [options]
   * @param {string} [options.apiKey] - API key for authentication (sent as X-API-Key header)
   * @param {number} [options.timeout] - Request timeout in milliseconds (default: 30000)
   */
  constructor(baseURL, options = {}) {
    this.baseURL = baseURL.replace(/\/+$/, '');
    this.apiKey = options.apiKey || null;
    this.timeout = options.timeout ?? 30000;
  }

  /**
   * Build common headers for requests.
   * @returns {Record<string, string>}
   */
  _headers() {
    const h = { 'Content-Type': 'application/json' };
    if (this.apiKey) {
      h['X-API-Key'] = this.apiKey;
    }
    return h;
  }

  /**
   * Internal fetch wrapper with timeout and error handling.
   * @param {string} path
   * @param {RequestInit} [init]
   * @returns {Promise<Response>}
   */
  async _fetch(path, init = {}) {
    const url = `${this.baseURL}${path}`;
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeout);

    try {
      const res = await fetch(url, {
        ...init,
        signal: controller.signal,
        headers: { ...this._headers(), ...init.headers },
      });
      return res;
    } catch (err) {
      if (err.name === 'AbortError') {
        throw new BoltQError(`Request to ${path} timed out after ${this.timeout}ms`, 0, null);
      }
      throw new BoltQError(`Request to ${path} failed: ${err.message}`, 0, null);
    } finally {
      clearTimeout(timer);
    }
  }

  /**
   * Internal helper: fetch + assert success + parse JSON.
   * @param {string} path
   * @param {RequestInit} [init]
   * @returns {Promise<any>}
   */
  async _request(path, init = {}) {
    const res = await this._fetch(path, init);
    if (!res.ok) {
      let body = null;
      try { body = await res.text(); } catch { /* ignore */ }
      throw new BoltQError(
        `BoltQ ${init.method || 'GET'} ${path} returned ${res.status}`,
        res.status,
        body,
      );
    }
    const text = await res.text();
    return text ? JSON.parse(text) : null;
  }

  // ---------------------------------------------------------------------------
  // Public API
  // ---------------------------------------------------------------------------

  /**
   * Publish a message to a queue topic.
   * @param {string} topic
   * @param {any} payload - Will be JSON-stringified if not already a string.
   * @param {Record<string, string>} [headers] - Optional message headers.
   * @returns {Promise<{id: string, topic: string}>}
   */
  async publish(topic, payload, headers) {
    return this._request('/publish', {
      method: 'POST',
      body: JSON.stringify({
        topic,
        payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
        headers: headers || {},
      }),
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
    return this._request('/publish/topic', {
      method: 'POST',
      body: JSON.stringify({
        topic,
        payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
        headers: headers || {},
      }),
    });
  }

  /**
   * Consume a single message from a queue topic.
   * Returns null when there are no messages available (204 No Content).
   * @param {string} topic
   * @returns {Promise<object|null>}
   */
  async consume(topic) {
    const res = await this._fetch(`/consume?topic=${encodeURIComponent(topic)}`, {
      method: 'GET',
    });

    if (res.status === 204) {
      return null;
    }

    if (!res.ok) {
      let body = null;
      try { body = await res.text(); } catch { /* ignore */ }
      throw new BoltQError(`BoltQ GET /consume returned ${res.status}`, res.status, body);
    }

    return res.json();
  }

  /**
   * Acknowledge (complete) a consumed message.
   * @param {string} messageId
   * @returns {Promise<void>}
   */
  async ack(messageId) {
    await this._request('/ack', {
      method: 'POST',
      body: JSON.stringify({ id: messageId }),
    });
  }

  /**
   * Negatively acknowledge a message so it can be redelivered.
   * @param {string} messageId
   * @returns {Promise<void>}
   */
  async nack(messageId) {
    await this._request('/nack', {
      method: 'POST',
      body: JSON.stringify({ id: messageId }),
    });
  }

  /**
   * Subscribe to a pub/sub topic via Server-Sent Events.
   *
   * The callback receives each message object as it arrives.
   * Returns an unsubscribe function that closes the connection.
   *
   * @param {string} topic
   * @param {string} subscriberId - Unique subscriber identifier.
   * @param {(message: object) => void} callback
   * @returns {() => void} unsubscribe function
   */
  subscribe(topic, subscriberId, callback) {
    const controller = new AbortController();
    const url = `${this.baseURL}/subscribe?topic=${encodeURIComponent(topic)}&id=${encodeURIComponent(subscriberId)}`;

    const headers = {};
    if (this.apiKey) {
      headers['X-API-Key'] = this.apiKey;
    }

    const run = async () => {
      try {
        const res = await fetch(url, {
          headers,
          signal: controller.signal,
        });

        if (!res.ok) {
          throw new BoltQError(
            `BoltQ subscribe returned ${res.status}`,
            res.status,
            null,
          );
        }

        const reader = res.body.getReader();
        const decoder = new TextDecoder();
        let buffer = '';

        while (true) {
          const { done, value } = await reader.read();
          if (done) break;

          buffer += decoder.decode(value, { stream: true });

          // Parse SSE frames: lines starting with "data: " separated by blank lines
          const parts = buffer.split('\n\n');
          // Keep the last (potentially incomplete) chunk in the buffer
          buffer = parts.pop() || '';

          for (const part of parts) {
            const lines = part.split('\n');
            let data = '';
            for (const line of lines) {
              if (line.startsWith('data: ')) {
                data += line.slice(6);
              } else if (line.startsWith('data:')) {
                data += line.slice(5);
              }
            }
            if (data) {
              try {
                callback(JSON.parse(data));
              } catch {
                // If not valid JSON, pass raw string
                callback(data);
              }
            }
          }
        }
      } catch (err) {
        if (err.name === 'AbortError') return; // expected on unsubscribe
        throw err;
      }
    };

    run().catch((err) => {
      // Surface unexpected errors; AbortError is swallowed above.
      console.error('[boltq-client] subscribe error:', err);
    });

    return () => controller.abort();
  }

  /**
   * Get broker statistics.
   * @returns {Promise<object>}
   */
  async stats() {
    return this._request('/stats');
  }

  /**
   * Get Prometheus-format metrics as a string.
   * @returns {Promise<string>}
   */
  async metrics() {
    const res = await this._fetch('/metrics');
    if (!res.ok) {
      throw new BoltQError(`BoltQ GET /metrics returned ${res.status}`, res.status, null);
    }
    return res.text();
  }

  /**
   * Health check. Returns true if the server is healthy, false otherwise.
   * @returns {Promise<boolean>}
   */
  async health() {
    try {
      const data = await this._request('/health');
      return data?.status === 'ok';
    } catch {
      return false;
    }
  }
}

export default BoltQClient;
