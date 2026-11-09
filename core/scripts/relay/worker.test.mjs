import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const source = await readFile(new URL("./worker.js", import.meta.url), "utf8");
const { default: worker } = await import(`data:text/javascript;base64,${Buffer.from(source).toString("base64")}`);
const env = { SECRET: "fixture" };
const request = (query = "", options) => new Request(`https://relay.example/?secret=fixture${query}`, options);

async function withFetch(mock, run) {
  const original = globalThis.fetch;
  globalThis.fetch = mock;
  try { await run(); } finally { globalThis.fetch = original; }
}

test("health responds locally without any upstream request or credential forwarding", async () => {
  await withFetch(async () => {
    assert.fail("health must never contact Guardian or any upstream");
  }, async () => {
    const response = await worker.fetch(request("&health=1", { headers: { Authorization: "Bearer fixture", Cookie: "fixture=1" } }), env);
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("cache-control"), "no-store");
    assert.deepEqual(await response.json(), { relay: "arman-vpn", version: 1, ready: true });
  });
});

test("worker health does not depend on Guardian being reachable", async () => {
  await withFetch(async () => { throw Error("Guardian is unavailable"); }, async () => {
    const response = await worker.fetch(request("&health=1"), env);
    assert.equal(response.status, 200);
    assert.equal((await response.json()).ready, true);
  });
});

test("health requires its secret and a GET", async () => {
  await withFetch(async () => { assert.fail("unexpected upstream request"); }, async () => {
    assert.equal((await worker.fetch(new Request("https://relay.example/?health=1&secret=wrong"), env)).status, 403);
    assert.equal((await worker.fetch(request("&health=1"))).status, 503);
    assert.equal((await worker.fetch(request("&health=1", { method: "POST" }), env)).status, 405);
  });
});

test("allowed requests retain auth and body but drop client IP and hop headers", async () => {
  await withFetch(async (target, options) => {
    assert.equal(String(target), "https://vpn.mozilla.org/api/v1/fpn/activate");
    assert.equal(options.method, "POST");
    assert.equal(options.redirect, "manual");
    assert.equal(options.headers.get("authorization"), "Bearer fixture");
    for (const name of ["x-forwarded-for", "cf-connecting-ip", "forwarded", "connection", "proxy-authorization"]) {
      assert.equal(options.headers.get(name), null, name);
    }
    assert.equal(new TextDecoder().decode(options.body), '{"fixture":true}');
    return new Response("fixture", { status: 302, headers: { Location: "https://vpn.mozilla.org/other" } });
  }, async () => {
    const response = await worker.fetch(request("&url=" + encodeURIComponent("https://vpn.mozilla.org/api/v1/fpn/activate"), {
      method: "POST", body: '{"fixture":true}', headers: {
        Authorization: "Bearer fixture", "X-Forwarded-For": "192.0.2.1", "CF-Connecting-IP": "192.0.2.1",
        Forwarded: "for=fixture", Connection: "close", "Proxy-Authorization": "fixture",
      },
    }), env);
    assert.equal(response.status, 302);
    assert.equal(response.headers.get("X-Arman-Relay"), "1");
  });
});

test("relay refuses unrelated hosts, non-HTTPS and URL credentials", async () => {
  await withFetch(async () => { assert.fail("unexpected upstream request"); }, async () => {
    for (const target of ["https://mozilla.org.evil.invalid/", "http://vpn.mozilla.org/", "https://u:p@vpn.mozilla.org/", "https://vpn.mozilla.org:8443/", "https://vpn.mozilla.org/#fragment"]) {
      const response = await worker.fetch(request("&url=" + encodeURIComponent(target)), env);
      assert.ok([400, 403].includes(response.status));
    }
  });
});

test("browser status is local and real forwarding errors return a bounded relay error", async () => {
  await withFetch(async () => { throw Error("fixture"); }, async () => {
    const page = await worker.fetch(request(), env);
    assert.equal(page.status, 200);
    assert.match(await page.text(), /Your relay is ready/);
    const response = await worker.fetch(request("&url=" + encodeURIComponent("https://vpn.mozilla.org/api/v1/fpn/token")), env);
    assert.equal(response.status, 502);
    assert.equal(response.headers.get("X-Relay-Error"), "upstream request failed");
  });
});
