import { Module } from '@nestjs/common';
import { OrderController } from './order.controller';
import { OrderConsumer } from './order.consumer';

@Module({
  controllers: [OrderController],
  providers: [OrderConsumer],
})
export class OrderModule {}
