const encoder = new TextEncoder();
const toleranceMs = 5 * 60 * 1000;
const hex = bytes => Array.from(new Uint8Array(bytes), b => b.toString(16).padStart(2, "0")).join("");
const bytesFrom64 = text => {
  const base = text.replace(/-/g, "+").replace(/_/g, "/");
  return Uint8Array.from(atob(base + "=".repeat((4 - base.length % 4) % 4)), c => c.charCodeAt(0));
};
const base64 = bytes => btoa(String.fromCharCode(...new Uint8Array(bytes))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const digest = async bytes => hex(await crypto.subtle.digest("SHA-256", bytes));
export async function keyId(publicKey) {
  const bytes = bytesFrom64(publicKey);
  if (bytes.length !== 32) throw new Error("Expected a 32-byte Ed25519 public key");
  return (await digest(bytes)).slice(0, 32);
}
async function input(id, timestamp, method, target, body) {
  const u = new URL(target);
  return encoder.encode([
    "discord-management-gateway", id, timestamp, method.toUpperCase(),
    u.hostname.toLowerCase() + (u.port ? ":" + u.port : ""),
    (u.pathname || "/") + u.search, await digest(body),
  ].join("\n"));
}
export async function signHeaders(privateSeed, publicKey, method, target, body, nowMs = Date.now()) {
  const seed = bytesFrom64(privateSeed);
  if (seed.length !== 32) throw new Error("Expected a 32-byte private seed");
  const prefix = Uint8Array.from([0x30,0x2e,0x02,0x01,0x00,0x30,0x05,0x06,0x03,0x2b,0x65,0x70,0x04,0x22,0x04,0x20]);
  const pkcs8 = new Uint8Array(prefix.length + seed.length);
  pkcs8.set(prefix); pkcs8.set(seed, prefix.length);
  const key = await crypto.subtle.importKey("pkcs8", pkcs8, {name:"Ed25519"}, false, ["sign"]);
  const id = await keyId(publicKey);
  const timestamp = String(Math.floor(nowMs / 1000));
  const raw = typeof body === "string" ? encoder.encode(body) : body;
  const signature = await crypto.subtle.sign("Ed25519", key, await input(id,timestamp,method,target,raw));
  return new Headers({
    "Content-Type":"application/json",
    "X-Gateway-Key-Id":id,
    "X-Gateway-Timestamp":timestamp,
    "X-Gateway-Signature":base64(signature),
  });
}
export async function verifyBytes(publicKey, method, target, body, headers, nowMs = Date.now()) {
  const expected = await keyId(publicKey);
  if (headers.get("X-Gateway-Key-Id") !== expected) throw new Error("Unknown signing key");
  const stamp = headers.get("X-Gateway-Timestamp") ?? "";
  if (!/^-?\d+$/.test(stamp)) throw new Error("Invalid signature timestamp");
  const issued = Number(stamp) * 1000;
  if (!Number.isSafeInteger(issued) || Math.abs(nowMs-issued) > toleranceMs) throw new Error("Expired signature");
  const signature = bytesFrom64(headers.get("X-Gateway-Signature") ?? "");
  if (signature.length !== 64) throw new Error("Invalid signature");
  const key = await crypto.subtle.importKey("raw", bytesFrom64(publicKey), {name:"Ed25519"}, false, ["verify"]);
  const raw = typeof body === "string" ? encoder.encode(body) : body;
  if (!await crypto.subtle.verify("Ed25519", key, signature, await input(expected,stamp,method,target,raw))) {
    throw new Error("Invalid signature");
  }
}
