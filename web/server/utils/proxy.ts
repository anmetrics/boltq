/**
 * Resolves the BoltQ admin API base URL.
 *
 * Order matters here. `nuxt.config.ts` reads `process.env.BOLTQ_ADMIN_URL` when
 * the app is *built*, which bakes whatever value the build machine had into the
 * bundle. An operator who then sets `BOLTQ_ADMIN_URL` on the *running* container
 * would silently keep talking to the build-time default — usually
 * `localhost:9090`, which is either nothing or, worse, some unrelated service.
 *
 * Reading the environment again at request time closes that trap, while
 * `runtimeConfig` still provides Nuxt's standard `NUXT_BOLTQ_URL` override and
 * the build-time default as a fallback.
 */
export function getBoltqUrl(): string {
  const fromEnv = process.env.BOLTQ_ADMIN_URL
  if (fromEnv) return stripTrailingSlash(fromEnv)

  const config = useRuntimeConfig()
  return stripTrailingSlash(config.boltqUrl || 'http://localhost:9090')
}

function stripTrailingSlash(url: string): string {
  return url.endsWith('/') ? url.slice(0, -1) : url
}
