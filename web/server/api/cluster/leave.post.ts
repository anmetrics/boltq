export default defineEventHandler(async (event) => {
  const url = getBoltqUrl()
  const body = await readBody(event)
  return await $fetch(`${url}/cluster/leave`, {
    method: 'POST',
    body,
  })
})
