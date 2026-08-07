import { Buffer } from "node:buffer";
import { createHash } from "node:crypto";
import {
  readFileSync,
  writeFileSync,
} from "node:fs";

const machMagic64 = 0xfeedfacf;
const machHeader64Size = 32;
const loadCommandUUID = 0x1b;
const loadCommandCodeSignature = 0x1d;

export function normalizeMachOUUID(path) {
  const binary = readFileSync(path);
  if (binary.readUInt32LE(0) !== machMagic64) {
    throw new Error(`expected a little-endian 64-bit Mach-O: ${path}`);
  }

  const commandCount = binary.readUInt32LE(16);
  let commandOffset = machHeader64Size;
  let uuidOffset;
  let signatureOffset;
  let signatureSize;

  for (let index = 0; index < commandCount; index += 1) {
    ensureRange(binary, commandOffset, 8);
    const command = binary.readUInt32LE(commandOffset);
    const commandSize = binary.readUInt32LE(commandOffset + 4);
    ensureRange(binary, commandOffset, commandSize);
    if (command === loadCommandUUID) {
      if (commandSize !== 24 || uuidOffset !== undefined) {
        throw new Error(`unexpected LC_UUID layout in ${path}`);
      }
      uuidOffset = commandOffset + 8;
    } else if (command === loadCommandCodeSignature) {
      if (commandSize !== 16 || signatureOffset !== undefined) {
        throw new Error(`unexpected LC_CODE_SIGNATURE layout in ${path}`);
      }
      signatureOffset = binary.readUInt32LE(commandOffset + 8);
      signatureSize = binary.readUInt32LE(commandOffset + 12);
    }
    commandOffset += commandSize;
  }

  if (
    uuidOffset === undefined ||
    signatureOffset === undefined ||
    signatureSize === undefined
  ) {
    throw new Error(`Mach-O lacks UUID or linker signature: ${path}`);
  }
  ensureRange(binary, signatureOffset, signatureSize);

  const normalized = Buffer.from(binary);
  normalized.fill(0, uuidOffset, uuidOffset + 16);
  normalized.fill(0, signatureOffset, signatureOffset + signatureSize);
  const uuid = createHash("sha256").update(normalized).digest().subarray(0, 16);
  uuid[6] = (uuid[6] & 0x0f) | 0x50;
  uuid[8] = (uuid[8] & 0x3f) | 0x80;
  uuid.copy(binary, uuidOffset);
  writeFileSync(path, binary);
}

function ensureRange(buffer, offset, size) {
  if (
    !Number.isInteger(offset) ||
    !Number.isInteger(size) ||
    offset < 0 ||
    size < 0 ||
    offset + size > buffer.length
  ) {
    throw new Error("invalid Mach-O load command range");
  }
}
