/**
 * BoltQ Node.js Client SDK - TypeScript Definitions
 */

export interface BoltQClientOptions {
  /** API key for authentication (sent as X-API-Key header). */
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
  [key: string]: unknown;
}

export declare class BoltQError extends Error {
  name: 'BoltQError';
  statusCode: number;
  body: string | null;
  constructor(message: string, statusCode: number, body: string | null);
}

export declare class BoltQClient {
  readonly baseURL: string;

  constructor(baseURL: string, options?: BoltQClientOptions);

  /**
   * Publish a message to a queue topic.
   * Non-string payloads are JSON-stringified automatically.
   */
  publish(
    topic: string,
    payload: unknown,
    headers?: Record<string, string>,
  ): Promise<PublishResult>;

  /**
   * Publish a message to a pub/sub topic.
   * Non-string payloads are JSON-stringified automatically.
   */
  publishTopic(
    topic: string,
    payload: unknown,
    headers?: Record<string, string>,
  ): Promise<PublishResult>;

  /**
   * Consume a single message from a queue topic.
   * Returns null when no messages are available.
   */
  consume(topic: string): Promise<Message | null>;

  /** Acknowledge (complete) a consumed message. */
  ack(messageId: string): Promise<void>;

  /** Negatively acknowledge a message for redelivery. */
  nack(messageId: string): Promise<void>;

  /**
   * Subscribe to a pub/sub topic via Server-Sent Events.
   * Returns an unsubscribe function that closes the connection.
   */
  subscribe(
    topic: string,
    subscriberId: string,
    callback: (message: Message) => void,
  ): () => void;

  /** Get broker statistics. */
  stats(): Promise<Record<string, unknown>>;

  /** Get Prometheus-format metrics as a raw string. */
  metrics(): Promise<string>;

  /** Health check. Returns true if the server is healthy. */
  health(): Promise<boolean>;
}

export default BoltQClient;
