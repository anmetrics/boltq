export default defineEventHandler(async (event) => {
  const url = getBoltqUrl()
  return await $fetch(`${url}/metrics`, {
    headers: { Accept: 'application/json' },
  })
})
