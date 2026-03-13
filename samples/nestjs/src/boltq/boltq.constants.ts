export interface BoltQModuleOptions {
  host: string;
  port: number;
  apiKey?: string;
  timeout?: number;
}

export const BOLTQ_OPTIONS = 'BOLTQ_OPTIONS';
