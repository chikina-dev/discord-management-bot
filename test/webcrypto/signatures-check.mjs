import { verifyBytes,signHeaders } from "./signatures.mjs";
let raw = "";
for await (const chunk of process.stdin) raw += chunk;
const vector = JSON.parse(raw);
const headers = new Headers(vector.headers);
await verifyBytes(vector.public_key,vector.method,vector.url,vector.body,headers,vector.now_ms);
let rejected = false;
try { await verifyBytes(vector.public_key,vector.method,vector.url,vector.body+" ",headers,vector.now_ms); }
catch { rejected = true; }
if (!rejected) throw new Error("Tampered body accepted");
const signed = await signHeaders(vector.seed,vector.public_key,vector.method,vector.url,vector.body,vector.now_ms);
process.stdout.write(JSON.stringify(Object.fromEntries(signed)));
