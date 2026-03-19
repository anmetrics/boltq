export default defineEventHandler(async () => {
  const url = getBoltqUrl()
  return await $fetch(`${url}/cache/stats`)
})
