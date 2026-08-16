const https = require('https');
const base = 'test-test.zeabur.app';

function req(method, path, headers) {
  return new Promise((resolve) => {
    const r = https.request({
      hostname: base, port: 443, path, method, headers: headers || {},
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }));
    });
    r.on('error', (e) => resolve({ error: e.message }));
    r.end();
  });
}

(async () => {
  // 1. 预检 OPTIONS 请求
  console.log('=== 预检: OPTIONS /v1/audio/speech (with Origin) ===');
  const r1 = await req('OPTIONS', '/v1/audio/speech', {
    'Origin': 'https://example.com',
    'Access-Control-Request-Method': 'POST',
    'Access-Control-Request-Headers': 'content-type, authorization',
  });
  console.log('Status:', r1.status);
  console.log('Headers:');
  for (const k of Object.keys(r1.headers)) {
    if (k.startsWith('access-control') || k === 'vary') {
      console.log('  ' + k + ': ' + r1.headers[k]);
    }
  }
  console.log('Body:', r1.body ? r1.body.toString('utf8') : '');

  // 2. 预检但无 Origin(同源或非浏览器)
  console.log('\n=== OPTIONS /v1/audio/speech (NO Origin) ===');
  const r2 = await req('OPTIONS', '/v1/audio/speech', {});
  console.log('Status:', r2.status);

  // 3. 实际 POST 请求
  console.log('\n=== POST /v1/audio/speech (with Origin) ===');
  const body = JSON.stringify({ model: 'tts-1', input: 'test', voice: 'alloy' });
  const r3 = await req('POST', '/v1/audio/speech', {
    'Origin': 'https://example.com',
    'Content-Type': 'application/json',
    'Authorization': 'Bearer sk-HRNfl15pp8e0KLewaydRCmvf2KfkOYd728yLJmuff5DkyaXd',
    'Content-Length': Buffer.byteLength(body),
  });
  if (r3.body) r3.write = r3.write || (() => {});
  console.log('Status:', r3.status);
  console.log('Access-Control-Allow-Origin:', r3.headers['access-control-allow-origin']);
  console.log('Vary:', r3.headers['vary']);
})();
