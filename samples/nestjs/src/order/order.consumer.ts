import { Injectable, OnModuleInit, OnModuleDestroy, Logger } from '@nestjs/common';
import { BoltQService } from '../boltq/boltq.service';

@Injectable()
export class OrderConsumer implements OnModuleInit, OnModuleDestroy {
  private readonly logger = new Logger(OrderConsumer.name);

  constructor(private readonly boltq: BoltQService) {}

  onModuleInit() {
    this.boltq.startConsumer('orders', async (msg) => {
      const payload = typeof msg.payload === 'string'
        ? JSON.parse(msg.payload)
        : msg.payload;

      this.logger.log(
        `Processing order: ${payload.product} x${payload.quantity} (msgId: ${msg.id})`,
      );

      // Simulate order processing
      await new Promise((r) => setTimeout(r, 100));

      this.logger.log(`Order ${msg.id} processed successfully`);
    });
  }

  onModuleDestroy() {
    this.boltq.stopConsumer('orders');
  }
}
