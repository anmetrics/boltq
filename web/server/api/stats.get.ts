export default defineEventHandler(async () => {
  const url = getBoltqUrl()
  return await $fetch(`${url}/stats`)
})
