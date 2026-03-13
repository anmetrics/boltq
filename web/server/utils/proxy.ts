export function getBoltqUrl(): string {
  const config = useRuntimeConfig()
  return config.boltqUrl || 'http://localhost:9090'
}
