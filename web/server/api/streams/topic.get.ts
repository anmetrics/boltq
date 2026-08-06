export default defineEventHandler(async (event) => {
  const url = getBoltqUrl()
  const query = getQuery(event)
  const params = new URLSearchParams()
  if (query.name) params.set('name', String(query.name))
  return await $fetch(`${url}/streams/topic?${params.toString()}`)
})
