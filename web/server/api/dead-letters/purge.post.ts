export default defineEventHandler(async (event) => {
  const url = getBoltqUrl()
  const body = await readBody(event)
  return await $fetch(`${url}/dead-letters/purge`, {
    method: 'POST',
    body,
  })
})
