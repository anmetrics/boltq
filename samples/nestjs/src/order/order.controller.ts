import { Controller, Post, Body, Get } from '@nestjs/common';
import { BoltQService } from '../boltq/boltq.service';

@Controller('orders')
export class OrderController {
  constructor(private readonly boltq: BoltQService) {}

  @Post()
  async createOrder(@Body() body: { product: string; quantity: number }) {
    const result = await this.boltq.publish('orders', {
      product: body.product,
      quantity: body.quantity,
      createdAt: new Date().toISOString(),
    });

    return { message: 'Order queued', messageId: result.id };
  }

  @Post('notify')
  async notifyAll(@Body() body: { event: string; data: any }) {
    const result = await this.boltq.publishTopic('order-events', {
      event: body.event,
      data: body.data,
      timestamp: new Date().toISOString(),
    });

    return { message: 'Event published', messageId: result.id };
  }

  @Get('stats')
  async getStats() {
    return this.boltq.stats();
  }

  @Get('health')
  async checkHealth() {
    const healthy = await this.boltq.health();
    return { boltq: healthy ? 'up' : 'down' };
  }
}
