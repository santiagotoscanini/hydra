// A frontend that knows nothing about hydra: it calls the api it was given
// in API_URL and reports the chain, in the same shape as mocksvc.
const http = require('node:http')

http
  .createServer(async (req, res) => {
    const out = { service: process.env.HYDRA_SERVICE, slot: process.env.HYDRA_SLOT, port: process.env.PORT }
    try {
      const r = await fetch(process.env.API_URL)
      out.upstreams = { API_URL: { url: process.env.API_URL, reply: await r.json() } }
    } catch (err) {
      out.upstreams = { API_URL: { url: process.env.API_URL, error: String(err.cause?.message ?? err) } }
    }
    res.setHeader('content-type', 'application/json')
    res.end(JSON.stringify(out))
  })
  .listen(process.env.PORT, '127.0.0.1')
