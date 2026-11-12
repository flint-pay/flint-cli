// Exercises the published SDK's default JWT verifier. Only trusted key fetching
// is replaced for mock tests; staging uses the SDK's normal JWKS discovery.
import { verifyWebhookAsync } from '@flintpay/node/webhooks';
import { createServer } from 'node:http';
import { writeFileSync } from 'node:fs';

const options = { baseUrl: 'https://api.withflintpay.com', endpointIds: [process.env.WEBHOOK_ENDPOINT_ID] };
if (process.env.FLINT_RECEIVER_JWKS) {
  const jwks = JSON.parse(process.env.FLINT_RECEIVER_JWKS);
  options.fetch = async (url) => {
    if (String(url) !== 'https://api.withflintpay.com/.well-known/jwks.json') throw new Error('Unexpected JWKS URL');
    return new Response(JSON.stringify(jwks), { headers: { 'Content-Type': 'application/json' } });
  };
} else {
  options.baseUrl = process.env.FLINT_BASE_URL;
}
const server = createServer(async (req, res) => {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  let valid = false, eventType = '';
  try {
    const verified = await verifyWebhookAsync(Buffer.concat(chunks), req.headers, options);
    valid = verified.signingMethod === 'jwt';
    eventType = verified.event.event_type;
  } catch { /* Do not expose bearer material in verification errors. */ }
  if (process.env.RECEIVER_RESULT) writeFileSync(process.env.RECEIVER_RESULT, JSON.stringify({valid_signature: valid, event_id: req.headers['webhook-id'], event_type: eventType}));
  res.writeHead(valid ? 204 : 400).end();
  if (process.env.RECEIVER_ONCE) server.close();
});
server.listen(Number(process.env.RECEIVER_PORT || 0), '127.0.0.1', () => {
  if (process.env.RECEIVER_READY) writeFileSync(process.env.RECEIVER_READY, '');
  console.log(`http://127.0.0.1:${server.address().port}`);
});
