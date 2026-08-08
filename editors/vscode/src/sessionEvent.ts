export const sessionDAPEventName = "bingo/session/v1";
export const sessionDAPEventVersion = 1;

export interface SessionAnnouncement {
  readonly sessionId: string;
}

export function decodeSessionAnnouncement(
  eventName: string,
  body: unknown,
): SessionAnnouncement | undefined {
  if (eventName !== sessionDAPEventName) {
    return undefined;
  }
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    throw new Error("bingo session event body must be an object");
  }
  const value = body as Record<string, unknown>;
  const keys = Object.keys(value).sort();
  if (keys.length !== 2 || keys[0] !== "sessionId" || keys[1] !== "version") {
    throw new Error("bingo session event body has unexpected fields");
  }
  if (value.version !== sessionDAPEventVersion) {
    throw new Error(
      `unsupported bingo session event version ${JSON.stringify(value.version)}`,
    );
  }
  if (
    typeof value.sessionId !== "string" ||
    value.sessionId.length === 0 ||
    value.sessionId.length > 128 ||
    !/^[A-Za-z0-9][A-Za-z0-9._-]*$/u.test(value.sessionId)
  ) {
    throw new Error("bingo session event has an invalid sessionId");
  }
  return { sessionId: value.sessionId };
}
