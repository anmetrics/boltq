export default defineEventHandler(async (event) => {
  const url = getBoltqUrl()
  const query = getQuery(event)
  const params = new URLSearchParams()
  if (query.topic) params.set('topic', String(query.topic))
  if (query.partition !== undefined) params.set('partition', String(query.partition))
  if (query.group) params.set('group', String(query.group))
  return await $fetch(`${url}/streams/cursors?${params.toString()}`)
})
