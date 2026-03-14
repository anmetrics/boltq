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
  publish(topic: string, payload: any, headers?: Record<string, string>, options?: { delay?: number; ttl?: number }): Promise<{ id: string; topic: string }>;

  /** Publish a message to a pub/sub topic. */
  publishTopic(topic: string, payload: any, headers?: Record<string, string>, options?: { delay?: number; ttl?: number }): Promise<{ id: string; topic: string }>;

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
}

export default BoltQClient;
