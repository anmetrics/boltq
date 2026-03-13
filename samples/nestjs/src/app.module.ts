import { Module } from '@nestjs/common';
import { BoltQModule } from './boltq/boltq.module';
import { OrderModule } from './order/order.module';

@Module({
  imports: [
    BoltQModule.forRoot({
      host: process.env.BOLTQ_HOST || 'localhost',
      port: parseInt(process.env.BOLTQ_PORT || '9091', 10),
      apiKey: process.env.BOLTQ_API_KEY || '',
    }),
    OrderModule,
  ],
})
export class AppModule {}
