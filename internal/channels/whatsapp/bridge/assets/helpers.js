// Helpers for the workingman WhatsApp bridge. Trimmed from hermes-agent's
// scripts/whatsapp-bridge/bridge_helpers.js (MIT, Nous Research); see NOTICE.md.
// Nothing here imports Baileys, so `node --test` can exercise it with no
// dependencies installed.

import { format } from 'util';

export function normalizeWhatsAppId(value) {
  if (!value) return '';
  // Baileys reports the account's own ids device-qualified (`<user>:<device>@lid`),
  // while inbound mentionedJid / contextInfo.participant are not. Drop the suffix
  // so both forms compare equal.
  return String(value).replace(/:\d+(?=@)/, '').replace(/:\d+$/, '');
}

function unwrapMessageEnvelopes(content) {
  let cur = content;
  // Envelopes nest (ephemeral wrapping viewOnce wrapping the payload); peel
  // until an inner message is reached.
  for (let i = 0; i < 8 && cur; i++) {
    const next =
      cur.ephemeralMessage?.message ??
      cur.viewOnceMessage?.message ??
      cur.viewOnceMessageV2?.message ??
      cur.documentWithCaptionMessage?.message;
    if (next === undefined) break;
    cur = next;
  }
  return cur;
}

export function getMessageContent(msg) {
  return unwrapMessageEnvelopes(msg?.message || {}) || {};
}

export function getContextInfo(messageContent) {
  if (!messageContent || typeof messageContent !== 'object') return {};
  for (const value of Object.values(messageContent)) {
    if (value && typeof value === 'object' && value.contextInfo) {
      return value.contextInfo;
    }
  }
  return {};
}

/**
 * Build the event the Go daemon long-polls for. Text only: captions of media
 * messages are kept (mediaType says what was attached) but the media itself is
 * never downloaded. Returns null when there is nothing for an agent to read.
 */
export function buildEvent({ msg, botIds = [] }) {
  const content = getMessageContent(msg);
  const contextInfo = getContextInfo(content);

  let body = '';
  let mediaType = '';
  if (content.conversation) {
    body = content.conversation;
  } else if (content.extendedTextMessage?.text) {
    body = content.extendedTextMessage.text;
  } else if (content.imageMessage) {
    body = content.imageMessage.caption || '';
    mediaType = 'image';
  } else if (content.videoMessage) {
    body = content.videoMessage.caption || '';
    mediaType = 'video';
  } else if (content.documentMessage) {
    body = content.documentMessage.caption || '';
    mediaType = 'document';
  }
  if (!body) return null;

  const key = msg.key || {};
  const chatId = key.remoteJid || '';
  const senderId = key.participant || chatId;
  // Baileys v7 carries the other form of the sender (group: participantAlt,
  // DM: remoteJidAlt), so a first-contact LID sender still matches a phone
  // allowlist before any lid-mapping file exists.
  const senderAltId = normalizeWhatsAppId(key.participantAlt || key.remoteJidAlt || '');
  const ts = Number(msg.messageTimestamp?.toString?.() ?? msg.messageTimestamp);

  return {
    messageId: key.id || '',
    chatId,
    senderId,
    senderAltId,
    senderName: msg.pushName || senderId.replace(/@.*/, ''),
    isGroup: chatId.endsWith('@g.us'),
    fromMe: Boolean(key.fromMe),
    body,
    mediaType,
    mentionedIds: Array.from(new Set((contextInfo.mentionedJid || []).map(normalizeWhatsAppId).filter(Boolean))),
    quotedMessageId: contextInfo.stanzaId || '',
    quotedParticipant: normalizeWhatsAppId(contextInfo.participant || ''),
    botIds,
    readReceiptKey: {
      remoteJid: chatId,
      id: key.id || '',
      participant: key.participant || '',
      fromMe: Boolean(key.fromMe),
    },
    timestamp: Number.isFinite(ts) && ts > 0 ? ts : Math.floor(Date.now() / 1000),
  };
}

/** Bounded LRU of recent inbound messages, so a reply can quote the original. */
export function createBoundedMessageStore(limit = 512) {
  const byId = new Map();

  function remember(msg) {
    const id = msg?.key?.id;
    if (!id) return;
    byId.delete(id);
    byId.set(id, msg);
    while (byId.size > limit) {
      byId.delete(byId.keys().next().value);
    }
  }

  function get(id) {
    if (!id || !byId.has(id)) return null;
    const msg = byId.get(id);
    byId.delete(id);
    byId.set(id, msg);
    return msg;
  }

  return { remember, get };
}

export function splitLongMessage(message, maxLength = 4096) {
  const text = String(message || '');
  if (!text) return [];
  if (!Number.isFinite(maxLength) || maxLength < 1 || text.length <= maxLength) {
    return [text];
  }
  const chunks = [];
  let remaining = text;
  while (remaining.length > maxLength) {
    let splitAt = remaining.lastIndexOf('\n', maxLength);
    if (splitAt < Math.floor(maxLength / 2)) {
      splitAt = remaining.lastIndexOf(' ', maxLength);
    }
    if (splitAt < 1) splitAt = maxLength;
    chunks.push(remaining.slice(0, splitAt).trimEnd());
    remaining = remaining.slice(splitAt).trimStart();
  }
  if (remaining) chunks.push(remaining);
  return chunks;
}

/**
 * Every (re)connect must go through this scheduler: a bare setTimeout around an
 * async start function turns a rejection into an unhandled one and a hang into
 * a permanently disconnected bridge.
 */
export function createReconnectScheduler(startFn, {
  retryDelayMs = 5000,
  log = console.log,
  setTimeoutFn = setTimeout,
} = {}) {
  function scheduleReconnect(delayMs) {
    setTimeoutFn(() => {
      Promise.resolve()
        .then(startFn)
        .catch((err) => {
          log(`Reconnect failed (${err?.message || err}). Retrying in ${Math.round(retryDelayMs / 1000)}s...`);
          scheduleReconnect(retryDelayMs);
        });
    }, delayMs);
  }
  return scheduleReconnect;
}

/**
 * fetchLatestBaileysVersion() is a plain fetch with no abort signal; bound it
 * and fall back to the last known-good version (or the Baileys default).
 */
export function createVersionResolver(fetchVersionFn, {
  timeoutMs = 15000,
  log = console.log,
} = {}) {
  let cachedVersion = null;
  return async function resolveVersion() {
    let timer = null;
    try {
      const { version } = await Promise.race([
        fetchVersionFn(),
        new Promise((_, reject) => {
          timer = setTimeout(() => reject(new Error('version fetch timed out')), timeoutMs);
        }),
      ]);
      cachedVersion = version;
    } catch (err) {
      log(`Baileys version fetch failed (${err?.message || err}); using ${cachedVersion ? 'cached version' : 'library default'}.`);
    } finally {
      if (timer) clearTimeout(timer);
    }
    return cachedVersion;
  };
}

const pad = (value, width = 2) => String(value).padStart(width, '0');

/** `2026-09-28 13:18:46,062` in local time. */
export function formatLogStamp(date) {
  const day = `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
  const time = `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
  return `${day} ${time},${pad(date.getMilliseconds(), 3)}`;
}

/** Stamp every console.log/warn/error line: the daemon captures them into a log file. */
export function installConsoleStamps(target = console) {
  for (const method of ['log', 'warn', 'error']) {
    const original = target[method].bind(target);
    target[method] = (...args) => {
      const text = format(...args);
      original('%s', text ? `${formatLogStamp(new Date())} ${text}` : text);
    };
  }
}
