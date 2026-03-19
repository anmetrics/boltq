/**
 * BoltQ Node.js Client SDK - TypeScript Definitions
 * TCP protocol client.
 */

import { EventEmitter } from 'events';

export interface BoltQClientOptions {
  /** API key for authentication. */
  apiKey?: string;
  /** Request timeout in milliseconds. Default: 30000. */
  timeout?: number;
  /** Enable automatic reconnection. Default: true. */
  autoReconnect?: boolean;
  /** Interval between reconnection attempts in milliseconds. Default: 2000. */
  reconnectInterval?: number;
}

export interface PublishResult {
  id: string;
  topic: string;
}

export interface Message {
  id: string;
  topic: string;
  payload: string;
  headers?: Record<string, string>;
  timestamp?: number;
  retry?: number;
  [key: string]: unknown;
}

export interface Consumer {
  stop(): void;
}

export declare class BoltQError extends Error {
  name: 'BoltQError';
  code: string;
  leader: string | null;
  leaderId: string | null;
  details: any;
  constructor(message: string, code?: string, details?: any);
}

export declare class BoltQClient extends EventEmitter {
  readonly host: string;
  readonly port: number;

  constructor(host: string, port: number, options?: BoltQClientOptions);

  /** Connect to the server and authenticate if apiKey is set. */
  connect(): Promise<void>;

  /** Disconnect from the server. */
  disconnect(): void;

  /** Publish a message to a queue topic. */
  publish(topic: string, payload: any, headers?: Record<string, string>, options?: { delay?: number; ttl?: number; priority?: number }): Promise<{ id: string; topic: string }>;

  /** Publish a message to a pub/sub topic. */
  publishTopic(topic: string, payload: any, headers?: Record<string, string>, options?: { delay?: number; ttl?: number; priority?: number }): Promise<{ id: string; topic: string }>;

  /** Consume a single message from a queue topic. Returns null if empty. */
  consume(topic: string): Promise<any | null>;

  /** Acknowledge a consumed message. */
  ack(messageId: string): Promise<void>;

  /** Negatively acknowledge a message for redelivery. */
  nack(messageId: string): Promise<void>;

  /** Ping the server. */
  ping(): Promise<void>;

  /** Get broker statistics. */
  stats(): Promise<any>;

  /** Set prefetch count for consumers. */
  setPrefetch(count: number): Promise<void>;

  /** Subscribe to a pub/sub topic. */
  subscribe(topic: string, subscriberId: string, options?: { durable?: boolean }, onMessage?: (msg: any) => void): Promise<void>;

  /** Health check. Returns true if ping succeeds. */
  health(): Promise<boolean>;

  /** Start polling a queue topic. Returns a handle with stop(). */
  startConsumer(
    topic: string,
    handler: (msg: Message) => Promise<void>,
    intervalMs?: number,
  ): Consumer;

  // --- Publisher Confirm & Exchange Routing ---

  /** Enable publisher confirm mode. */
  enableConfirm(): Promise<void>;

  /** Declare an exchange. */
  exchangeDeclare(name: string, type?: 'direct' | 'fanout' | 'topic' | 'headers', durable?: boolean): Promise<void>;

  /** Delete an exchange. */
  exchangeDelete(name: string): Promise<void>;

  /** Bind a queue to an exchange. */
  bindQueue(exchange: string, queue: string, bindingKey?: string, options?: { headers?: Record<string, string>; matchAll?: boolean }): Promise<void>;

  /** Unbind a queue from an exchange. */
  unbindQueue(exchange: string, queue: string, bindingKey?: string): Promise<void>;

  /** Publish a message to an exchange with a routing key. */
  publishToExchange(exchange: string, routingKey: string, payload: any, headers?: Record<string, string>, options?: { priority?: number }): Promise<{ id: string }>;

  // --- Cache / KV Store ---

  /** Get a value from cache. */
  cacheGet(key: string): Promise<CacheEntry>;

  /** Set a key-value pair in cache. ttl in milliseconds, 0 = no expiry. */
  cacheSet(key: string, value: any, ttl?: number): Promise<{ status: string; key: string }>;

  /** Delete a key from cache. */
  cacheDel(key: string): Promise<{ status: string; deleted: boolean }>;

  /** List keys matching a glob pattern. */
  cacheKeys(pattern?: string): Promise<{ keys: string[]; count: number }>;

  /** Check if a key exists. */
  cacheExists(key: string): Promise<{ key: string; exists: boolean }>;

  /** Set TTL on an existing key (milliseconds). Use 0 to remove TTL. */
  cacheExpire(key: string, ttl: number): Promise<{ status: string; key: string; found: boolean }>;

  /** Atomically increment a numeric value. Creates key if not exists. */
  cacheIncr(key: string, delta?: number): Promise<{ key: string; value: number }>;

  /** Flush all cache entries. */
  cacheFlush(): Promise<{ status: string; removed: number }>;

  /** Get cache statistics. */
  cacheStats(): Promise<CacheStatsResult>;
}

export interface CacheEntry {
  key: string;
  value: any;
  ttl: number;
  created_at: number;
}

export interface CacheStatsResult {
  enabled: boolean;
  stats: {
    key_count: number;
    memory_used: number;
    hits: number;
    misses: number;
    sets: number;
    deletes: number;
    expired: number;
  };
}

export default BoltQClient;
