import {
  Injectable,
  Inject,
  OnModuleDestroy,
  Logger,
} from '@nestjs/common';
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
  timestamp: number;
  retry: number;
}

@Injectable()
export class BoltQService implements OnModuleDestroy {
  private readonly logger = new Logger(BoltQService.name);
  private readonly baseURL: string;
  private readonly apiKey: string;
  private readonly timeout: number;
  private readonly pollers: Map<string, AbortController> = new Map();

  constructor(@Inject(BOLTQ_OPTIONS) options: BoltQModuleOptions) {
    this.baseURL = options.baseURL.replace(/\/+$/, '');
    this.apiKey = options.apiKey || '';
    this.timeout = options.timeout ?? 30000;
  }

  onModuleDestroy() {
    for (const [key, controller] of this.pollers) {
      controller.abort();
      this.pollers.delete(key);
    }
  }

  private headers(): Record<string, string> {
    const h: Record<string, string> = { 'Content-Type': 'application/json' };
    if (this.apiKey) h['X-API-Key'] = this.apiKey;
    return h;
  }

  private async request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), this.timeout);

    try {
      const res = await fetch(`${this.baseURL}${path}`, {
        ...init,
        signal: controller.signal,
        headers: { ...this.headers(), ...init.headers },
      });

      if (!res.ok) {
        const body = await res.text().catch(() => '');
        throw new Error(`BoltQ ${init.method || 'GET'} ${path} returned ${res.status}: ${body}`);
      }

      const text = await res.text();
      return text ? JSON.parse(text) : (null as T);
    } finally {
      clearTimeout(timer);
    }
  }

  async publish(topic: string, payload: any, headers?: Record<string, string>): Promise<PublishResponse> {
    return this.request<PublishResponse>('/publish', {
      method: 'POST',
      body: JSON.stringify({
        topic,
        payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
        headers: headers || {},
      }),
    });
  }

  async publishTopic(topic: string, payload: any, headers?: Record<string, string>): Promise<PublishResponse> {
    return this.request<PublishResponse>('/publish/topic', {
      method: 'POST',
      body: JSON.stringify({
        topic,
        payload: typeof payload === 'string' ? payload : JSON.stringify(payload),
        headers: headers || {},
      }),
    });
  }

  async consume(topic: string): Promise<ConsumeResponse | null> {
    const res = await fetch(
      `${this.baseURL}/consume?topic=${encodeURIComponent(topic)}`,
      { headers: this.headers() },
    );
    if (res.status === 204) return null;
    if (!res.ok) throw new Error(`BoltQ consume returned ${res.status}`);
    return res.json();
  }

  async ack(messageId: string): Promise<void> {
    await this.request('/ack', {
      method: 'POST',
      body: JSON.stringify({ id: messageId }),
    });
  }

  async nack(messageId: string): Promise<void> {
    await this.request('/nack', {
      method: 'POST',
      body: JSON.stringify({ id: messageId }),
    });
  }

  /**
   * Start polling a queue topic. Calls `handler` for each message.
   * If the handler resolves, the message is acked; if it rejects, the message is nacked.
   */
  startConsumer(
    topic: string,
    handler: (msg: ConsumeResponse) => Promise<void>,
    intervalMs = 1000,
  ): void {
    if (this.pollers.has(topic)) {
      this.logger.warn(`Consumer for topic "${topic}" is already running`);
      return;
    }

    const controller = new AbortController();
    this.pollers.set(topic, controller);

    const poll = async () => {
      while (!controller.signal.aborted) {
        try {
          const msg = await this.consume(topic);
          if (msg) {
            try {
              await handler(msg);
              await this.ack(msg.id);
            } catch (err) {
              this.logger.error(`Handler error for message ${msg.id}: ${err}`);
              await this.nack(msg.id).catch(() => {});
            }
          } else {
            await new Promise((r) => setTimeout(r, intervalMs));
          }
        } catch (err) {
          if (controller.signal.aborted) break;
          this.logger.error(`Poll error on topic "${topic}": ${err}`);
          await new Promise((r) => setTimeout(r, intervalMs * 2));
        }
      }
    };

    poll();
    this.logger.log(`Consumer started for topic "${topic}"`);
  }

  stopConsumer(topic: string): void {
    const controller = this.pollers.get(topic);
    if (controller) {
      controller.abort();
      this.pollers.delete(topic);
      this.logger.log(`Consumer stopped for topic "${topic}"`);
    }
  }

  async health(): Promise<boolean> {
    try {
      const data = await this.request<{ status: string }>('/health');
      return data?.status === 'ok';
    } catch {
      return false;
    }
  }

  async stats(): Promise<any> {
    return this.request('/stats');
  }
}
