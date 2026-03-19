export default defineEventHandler(async (event) => {
  const url = getBoltqUrl()
  const query = getQuery(event)
  return await $fetch(`${url}/cache/get?key=${encodeURIComponent(String(query.key))}`)
})
