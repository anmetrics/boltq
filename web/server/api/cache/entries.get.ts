export default defineEventHandler(async (event) => {
  const url = getBoltqUrl()
  const query = getQuery(event)
  const params = new URLSearchParams()
  if (query.pattern) params.set('pattern', String(query.pattern))
  if (query.search) params.set('search', String(query.search))
  return await $fetch(`${url}/cache/entries?${params.toString()}`)
})
