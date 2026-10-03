#!/usr/bin/env node
/**
 * workingman WhatsApp bridge
 *
 * A local Node process that links to WhatsApp as a Baileys "linked device" and
 * exposes a small HTTP API on 127.0.0.1 for the workingman daemon. Trimmed from
 * hermes-agent's scripts/whatsapp-bridge/bridge.js (MIT, Nous Research); see
 * NOTICE.md.
 *
 * The bridge is deliberately policy-free: it forwards every text message it
 * sees (including the account's own) and the daemon's AccessPolicy decides
 * which ones an agent may read.
 *
 * Endpoints (every request needs `Authorization: Bearer $WORKINGMAN_BRIDGE_TOKEN`
 * when that variable is set):
 *   GET  /messages?timeout=<s>  long-poll: queued events, waiting up to timeout
 *   POST /send                  { chatId, message, replyTo? } -> { messageId, messageIds }
 *   POST /typing                { chatId }
 *   POST /read                  { key: { remoteJid, id, participant?, fromMe? } }
 *   GET  /health                connection state, own ids, queue length
 *
 * Usage:
 *   node bridge.js --port 3000 --session ~/.workingman/whatsapp/session
 *   node bridge.js --pair --session ...    show a QR code, save creds, exit 0
 *
 * Exit codes: 0 pairing complete, 1 error, 78 WhatsApp logged this session out
 * (re-pair; the daemon does not restart on 78).
 */

import { makeWASocket, useMultiFileAuthState, DisconnectReason, fetchLatestBaileysVersion, generateMessageIDV2 } from '@whiskeysockets/baileys';
import express from 'express';
import pino from 'pino';
import path from 'path';
import { mkdirSync, readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import { createHash, timingSafeEqual } from 'crypto';
import qrcode from 'qrcode-terminal';
import { createOutboundIdTracker } from './outbound_ids.js';
import {
  buildEvent,
  createBoundedMessageStore,
  createReconnectScheduler,
  createVersionResolver,
  installConsoleStamps,
  normalizeWhatsAppId,
  splitLongMessage,
} from './helpers.js';

const EXIT_LOGGED_OUT = 78;

const args = process.argv.slice(2);
function getArg(name, defaultVal) {
  const idx = args.indexOf(`--${name}`);
  return idx !== -1 && args[idx + 1] ? args[idx + 1] : defaultVal;
}

const PAIR = args.includes('--pair');
// Pairing is interactive: a timestamp on the first row of the QR art would skew it.
if (!PAIR) installConsoleStamps();

const PORT = parseInt(getArg('port', '3000'), 10);
const SESSION_DIR = getArg('session', path.join(process.env.HOME || '~', '.workingman', 'whatsapp', 'session'));
const TOKEN = process.env.WORKINGMAN_BRIDGE_TOKEN || '';
const MAX_MESSAGE_LENGTH = parseInt(process.env.WHATSAPP_MAX_MESSAGE_LENGTH || '4096', 10);
const CHUNK_DELAY_MS = parseInt(process.env.WHATSAPP_CHUNK_DELAY_MS || '300', 10);
// Baileys occasionally hangs forever on a send; fail fast so the daemon can report it.
const SEND_TIMEOUT_MS = parseInt(process.env.WHATSAPP_SEND_TIMEOUT_MS || '60000', 10);
const MAX_QUEUE_SIZE = 200;

// Hash of this script, reported in /health so the daemon can tell a long-lived
// bridge predating the current bridge.js from a fresh one.
let SCRIPT_HASH = '';
try {
  SCRIPT_HASH = createHash('sha256')
    .update(readFileSync(fileURLToPath(import.meta.url)))
    .digest('hex')
    .slice(0, 16);
} catch {}

mkdirSync(SESSION_DIR, { recursive: true });

const logger = pino({ level: 'warn' });

// Ids of messages we sent. The daemon also tracks these, but the bridge drops
// the echo here too so our own replies never even reach the queue.
const recentlySentIds = createOutboundIdTracker(512);
// Recent inbound messages, so /send can quote one when asked to reply.
const messageStore = createBoundedMessageStore(512);

let sock = null;
let connectionState = 'disconnected';

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// --- Send queue: Baileys must never see overlapping sendMessage() calls on one
//     socket (hermes-agent#33360: cross-chat misdelivery). ---
let sendQueue = Promise.resolve();
function enqueueSend(fn) {
  const task = sendQueue.then(() => fn(), () => fn());
  sendQueue = task.catch(() => {});
  return task;
}

function sendWithTimeout(chatId, payload, options = {}) {
  let timer;
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`sendMessage timed out after ${SEND_TIMEOUT_MS / 1000}s`)), SEND_TIMEOUT_MS);
  });
  return enqueueSend(() =>
    Promise.race([sock.sendMessage(chatId, payload, options), timeout]).finally(() => clearTimeout(timer))
  );
}

// --- Event queue with long-poll waiters ---
const queue = [];
let waiters = [];

function enqueueEvent(event) {
  queue.push(event);
  if (queue.length > MAX_QUEUE_SIZE) queue.shift();
  flushWaiters();
}

function flushWaiters() {
  while (waiters.length > 0 && queue.length > 0) {
    waiters.shift().deliver(queue.splice(0, queue.length));
  }
}

function connectedUser() {
  return {
    id: normalizeWhatsAppId(sock?.user?.id) || null,
    lid: normalizeWhatsAppId(sock?.user?.lid) || null,
    name: sock?.user?.name || sock?.user?.verifiedName || null,
  };
}

const scheduleReconnect = createReconnectScheduler(() => startSocket());
const getWAVersion = createVersionResolver(fetchLatestBaileysVersion);

async function startSocket() {
  const { state, saveCreds } = await useMultiFileAuthState(SESSION_DIR);
  const version = await getWAVersion();

  sock = makeWASocket({
    ...(version ? { version } : {}),
    auth: state,
    logger,
    printQRInTerminal: false,
    browser: ['Workingman', 'Chrome', '120.0'],
    syncFullHistory: false,
    markOnlineOnConnect: false,
    // Required for Baileys 7.x: without it, messages that need E2EE session
    // re-establishment are silently dropped. A placeholder completes the retry handshake.
    getMessage: async () => ({ conversation: '' }),
  });

  sock.ev.on('creds.update', () => { saveCreds(); });

  sock.ev.on('connection.update', (update) => {
    const { connection, lastDisconnect, qr } = update;

    if (qr) {
      if (PAIR) {
        console.log('\nScan this QR code with WhatsApp on your phone (Settings > Linked devices > Link a device):\n');
        qrcode.generate(qr, { small: true }, (code) => process.stdout.write(`${code}\n`));
        console.log('\nWaiting for scan...\n');
      } else {
        // A running daemon cannot show a QR: the session is not paired.
        console.error('Not paired: run `orch whatsapp pair` to link this number.');
        process.exit(EXIT_LOGGED_OUT);
      }
    }

    if (connection === 'close') {
      const reason = lastDisconnect?.error?.output?.statusCode;
      connectionState = 'disconnected';
      if (reason === DisconnectReason.loggedOut) {
        console.error('Logged out by WhatsApp. Run `orch whatsapp pair` to re-link.');
        process.exit(EXIT_LOGGED_OUT);
      }
      // 515 = restart requested, normal right after pairing.
      console.log(reason === 515
        ? 'WhatsApp requested a restart (515). Reconnecting...'
        : `Connection closed (reason: ${reason}). Reconnecting in 3s...`);
      scheduleReconnect(reason === 515 ? 1000 : 3000);
    } else if (connection === 'open') {
      connectionState = 'connected';
      console.log('WhatsApp connected.');
      if (PAIR) {
        console.log('Pairing complete. Credentials saved.');
        // Give Baileys a moment to flush creds, then exit cleanly.
        setTimeout(() => process.exit(0), 2000);
      }
    }
  });

  sock.ev.on('messages.upsert', ({ messages, type }) => {
    if (PAIR) return;
    // Your own messages commonly arrive as 'append' rather than 'notify'.
    if (type !== 'notify' && type !== 'append') return;

    const botIds = Array.from(new Set([
      normalizeWhatsAppId(sock.user?.id),
      normalizeWhatsAppId(sock.user?.lid),
    ].filter(Boolean)));

    for (const msg of messages) {
      if (!msg.message || !msg.key) continue;
      if (msg.key.fromMe && recentlySentIds.has(msg.key.id)) continue; // echo of our own send
      const event = buildEvent({ msg, botIds });
      if (event) {
        messageStore.remember(msg);
        enqueueEvent(event);
      }
    }
  });
}

// --- HTTP server ---
const app = express();
app.use(express.json({ limit: '1mb' }));

// Host-header validation defends against DNS rebinding (GHSA-ppp5-vxwm-4cf7):
// reject any request whose Host is not a loopback alias.
const ACCEPTED_HOSTS = new Set(['localhost', '127.0.0.1', '[::1]', '::1']);
app.use((req, res, next) => {
  const raw = (req.headers.host || '').trim();
  if (!raw) return res.status(400).json({ error: 'Missing Host header' });
  const hostOnly = (raw.includes(':') ? raw.substring(0, raw.lastIndexOf(':')) : raw)
    .replace(/^\[|\]$/g, '').toLowerCase();
  if (!ACCEPTED_HOSTS.has(hostOnly)) {
    return res.status(400).json({ error: 'Invalid Host header. Bridge accepts loopback hosts only.' });
  }
  next();
});

// Bearer token: any other local process can reach 127.0.0.1, so require the
// per-run secret the daemon generated.
app.use((req, res, next) => {
  if (!TOKEN) return next();
  const given = Buffer.from((req.headers.authorization || '').replace(/^Bearer\s+/i, ''));
  const want = Buffer.from(TOKEN);
  if (given.length !== want.length || !timingSafeEqual(given, want)) {
    return res.status(401).json({ error: 'unauthorized' });
  }
  next();
});

const connected = () => sock && connectionState === 'connected';

app.get('/messages', (req, res) => {
  if (queue.length > 0) return res.json(queue.splice(0, queue.length));
  const timeoutS = Math.min(Math.max(parseFloat(req.query.timeout) || 0, 0), 60);
  if (timeoutS === 0) return res.json([]);

  const waiter = {
    deliver(events) {
      clearTimeout(waiter.timer);
      res.json(events);
    },
  };
  waiter.timer = setTimeout(() => {
    waiters = waiters.filter((w) => w !== waiter);
    res.json([]);
  }, timeoutS * 1000);
  // A poller that went away must not swallow the next batch.
  res.on('close', () => {
    clearTimeout(waiter.timer);
    waiters = waiters.filter((w) => w !== waiter);
  });
  waiters.push(waiter);
});

app.post('/send', async (req, res) => {
  if (!connected()) return res.status(503).json({ error: 'Not connected to WhatsApp' });

  const { chatId, message, replyTo } = req.body || {};
  if (!chatId || !message) return res.status(400).json({ error: 'chatId and message are required' });

  const messageIds = [];
  try {
    const chunks = splitLongMessage(message, MAX_MESSAGE_LENGTH);
    for (let i = 0; i < chunks.length; i += 1) {
      // Assign the id up front and remember it before sending, so the echo is
      // recognised even if it is delivered before sendMessage() resolves.
      const messageId = generateMessageIDV2(sock.user?.id);
      recentlySentIds.remember(messageId);
      const options = { messageId };
      if (i === 0 && replyTo) {
        // Baileys wants the whole quoted message; without it, send a plain message.
        const quoted = messageStore.get(replyTo);
        if (quoted) options.quoted = quoted;
      }
      const sent = await sendWithTimeout(chatId, { text: chunks[i] }, options);
      const id = sent?.key?.id || messageId;
      recentlySentIds.remember(id);
      messageIds.push(id);
      if (i < chunks.length - 1 && CHUNK_DELAY_MS > 0) await sleep(CHUNK_DELAY_MS);
    }
    res.json({ success: true, messageId: messageIds[0] || '', messageIds });
  } catch (err) {
    console.error('send failed:', err.message);
    // messageIds so far were delivered; report them so the daemon can suppress their echoes.
    res.status(500).json({ error: err.message, messageIds });
  }
});

app.post('/typing', async (req, res) => {
  if (!connected()) return res.status(503).json({ error: 'Not connected' });
  const { chatId } = req.body || {};
  if (!chatId) return res.status(400).json({ error: 'chatId required' });
  try {
    await sock.sendPresenceUpdate('composing', chatId);
    res.json({ success: true });
  } catch {
    res.json({ success: false });
  }
});

// Read receipts: the daemon only calls this for messages its policy admitted.
app.post('/read', async (req, res) => {
  if (!connected()) return res.status(503).json({ error: 'Not connected' });
  const key = req.body?.key;
  if (!key || key.fromMe || !key.id || !key.remoteJid) return res.json({ success: true, marked: false });
  try {
    await sock.readMessages([key]);
    res.json({ success: true, marked: true });
  } catch (err) {
    console.warn('read receipt failed:', err.message);
    res.status(500).json({ error: 'Failed to send read receipt' });
  }
});

app.get('/health', (req, res) => {
  res.json({
    status: connectionState,
    queueLength: queue.length,
    uptime: process.uptime(),
    scriptHash: SCRIPT_HASH,
    user: connectedUser(),
  });
});

// --- Start ---
if (PAIR) {
  console.log('WhatsApp pairing mode');
  console.log(`Session: ${SESSION_DIR}`);
  console.log();
  startSocket().catch((err) => {
    console.error(err);
    process.exit(1);
  });
} else {
  app.listen(PORT, '127.0.0.1', () => {
    console.log(`WhatsApp bridge listening on 127.0.0.1:${PORT}`);
    console.log(`Session stored in: ${SESSION_DIR}`);
    scheduleReconnect(0);
  });
}
