import {
  Injectable,
  Inject,
  OnModuleInit,
  OnModuleDestroy,
  Logger,
} from '@nestjs/common';
import { BoltQClient } from '../../../../client/nodejs/index.js';
import { BOLTQ_OPTIONS, BoltQModuleOptions } from './boltq.constants';

interface PublishResponse {
  id: string;
  topic: string;
}

interface ConsumeResponse {
  id: string;
  topic: string;
  payload: any;
  headers?: Record<string, string>;
  timestamp?: number;
  retry?: number;
}

@Injectable()
export class BoltQService implements OnModuleInit, OnModuleDestroy {
  private readonly logger = new Logger(BoltQService.name);
  private readonly client: BoltQClient;
  private readonly consumers: Map<string, { stop: () => void }> = new Map();

  constructor(@Inject(BOLTQ_OPTIONS) options: BoltQModuleOptions) {
    this.client = new BoltQClient(options.host, options.port, {
      apiKey: options.apiKey,
      timeout: options.timeout,
    });
  }

  async onModuleInit() {
    await this.client.connect();
    this.logger.log('Connected to BoltQ server');
  }

  onModuleDestroy() {
    for (const [topic, consumer] of this.consumers) {
      consumer.stop();
      this.consumers.delete(topic);
    }
    this.client.disconnect();
  }

  async publish(topic: string, payload: any, headers?: Record<string, string>): Promise<PublishResponse> {
    return this.client.publish(topic, payload, headers) as Promise<PublishResponse>;
  }

  async publishTopic(topic: string, payload: any, headers?: Record<string, string>): Promise<PublishResponse> {
    return this.client.publishTopic(topic, payload, headers) as Promise<PublishResponse>;
  }

  async consume(topic: string): Promise<ConsumeResponse | null> {
    return this.client.consume(topic) as Promise<ConsumeResponse | null>;
  }

  async ack(messageId: string): Promise<void> {
    await this.client.ack(messageId);
  }

  async nack(messageId: string): Promise<void> {
    await this.client.nack(messageId);
  }

  startConsumer(
    topic: string,
    handler: (msg: ConsumeResponse) => Promise<void>,
    intervalMs = 1000,
  ): void {
    if (this.consumers.has(topic)) {
      this.logger.warn(`Consumer for topic "${topic}" is already running`);
      return;
    }

    const consumer = this.client.startConsumer(topic, handler, intervalMs);
    this.consumers.set(topic, consumer);
    this.logger.log(`Consumer started for topic "${topic}"`);
  }

  stopConsumer(topic: string): void {
    const consumer = this.consumers.get(topic);
    if (consumer) {
      consumer.stop();
      this.consumers.delete(topic);
      this.logger.log(`Consumer stopped for topic "${topic}"`);
    }
  }

  async health(): Promise<boolean> {
    return this.client.health();
  }

  async stats(): Promise<any> {
    return this.client.stats();
  }
}
