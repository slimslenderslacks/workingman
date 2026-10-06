import test from 'node:test';
import assert from 'node:assert/strict';
import { buildEvent, normalizeWhatsAppId, splitLongMessage } from './helpers.js';
import { createOutboundIdTracker } from './outbound_ids.js';

test('normalizeWhatsAppId drops the device suffix', () => {
  assert.equal(normalizeWhatsAppId('15551234567:12@s.whatsapp.net'), '15551234567@s.whatsapp.net');
  assert.equal(normalizeWhatsAppId('99@lid'), '99@lid');
  assert.equal(normalizeWhatsAppId(''), '');
});

test('buildEvent extracts text, sender alias and mentions', () => {
  const ev = buildEvent({
    botIds: ['1@s.whatsapp.net'],
    msg: {
      key: { id: 'A1', remoteJid: '55@lid', remoteJidAlt: '15551234567@s.whatsapp.net', fromMe: false },
      pushName: 'Sam',
      messageTimestamp: 1700000000,
      message: {
        extendedTextMessage: {
          text: 'hi @1',
          contextInfo: { mentionedJid: ['1:3@s.whatsapp.net'], stanzaId: 'Q1', participant: '9:1@lid' },
        },
      },
    },
  });
  assert.equal(ev.body, 'hi @1');
  assert.equal(ev.senderAltId, '15551234567@s.whatsapp.net');
  assert.deepEqual(ev.mentionedIds, ['1@s.whatsapp.net']);
  assert.equal(ev.quotedParticipant, '9@lid');
  assert.equal(ev.fromMe, false);
  assert.equal(ev.timestamp, 1700000000);
});

test('buildEvent skips messages with no text', () => {
  const msg = { key: { id: 'B', remoteJid: '1@s.whatsapp.net' }, message: { stickerMessage: {} } };
  assert.equal(buildEvent({ msg }), null);
});

test('buildEvent keeps a media caption and unwraps ephemeral envelopes', () => {
  const ev = buildEvent({
    msg: {
      key: { id: 'C', remoteJid: '1@s.whatsapp.net' },
      message: { ephemeralMessage: { message: { imageMessage: { caption: 'look' } } } },
    },
  });
  assert.equal(ev.body, 'look');
  assert.equal(ev.mediaType, 'image');
});

test('splitLongMessage prefers newlines and respects the limit', () => {
  const chunks = splitLongMessage('aaaa\nbbbb\ncccc', 9);
  assert.deepEqual(chunks, ['aaaa\nbbbb', 'cccc']);
  assert.deepEqual(splitLongMessage('short', 100), ['short']);
  assert.deepEqual(splitLongMessage('', 100), []);
});

test('outbound id tracker evicts the oldest id', () => {
  const t = createOutboundIdTracker(2);
  t.remember('a'); t.remember('b'); t.remember('c');
  assert.equal(t.has('a'), false);
  assert.equal(t.has('c'), true);
});
