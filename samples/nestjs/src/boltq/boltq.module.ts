import { Module, DynamicModule, Global } from '@nestjs/common';
import { BoltQService } from './boltq.service';
import { BOLTQ_OPTIONS, BoltQModuleOptions } from './boltq.constants';

export { BOLTQ_OPTIONS, BoltQModuleOptions };

@Global()
@Module({})
export class BoltQModule {
  static forRoot(options: BoltQModuleOptions): DynamicModule {
    return {
      module: BoltQModule,
      providers: [
        { provide: BOLTQ_OPTIONS, useValue: options },
        BoltQService,
      ],
      exports: [BoltQService],
    };
  }
}
