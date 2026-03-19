export default defineEventHandler(async (event) => {
  const url = getBoltqUrl()
  const body = await readBody(event)
  return await $fetch(`${url}/cache/del`, { method: 'POST', body })
})
