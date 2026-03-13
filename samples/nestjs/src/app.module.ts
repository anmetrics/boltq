import { Module } from '@nestjs/common';
import { BoltQModule } from './boltq/boltq.module';
import { OrderModule } from './order/order.module';

@Module({
  imports: [
    BoltQModule.forRoot({
      baseURL: process.env.BOLTQ_URL || 'http://localhost:9090',
      apiKey: process.env.BOLTQ_API_KEY || '',
    }),
    OrderModule,
  ],
})
export class AppModule {}
