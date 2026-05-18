/**
 * Lark (Feishu) integration types — Phase 1 (workspace binding only).
 *
 * The server populates `supported_events` from its internal `supportedLarkEvents`
 * list — when the server gains support for a new event kind, the UI's
 * checklist updates automatically without a frontend change.
 */

export type LarkEventKind =
  | "issue:created"
  | "issue:updated"
  | "task:completed"
  | "task:failed"
  | "comment:created";

export interface LarkBindingResponse {
  /** Whether a binding row exists for this workspace. */
  bound: boolean;
  /** Whether the server has Lark credentials (LARK_APP_ID etc.) set. */
  configured: boolean;
  /** Lark open_chat_id messages are posted into. Empty when `bound` is false. */
  chat_id?: string;
  /** Event kinds currently enabled for this workspace. */
  enabled_events: string[];
  /** The catalogue of event kinds this server knows how to render. */
  supported_events: string[];
  created_at?: string;
  updated_at?: string;
}

export interface UpsertLarkBindingRequest {
  chat_id: string;
  enabled_events: string[];
}

export interface PatchLarkBindingRequest {
  enabled_events: string[];
}

/**
 * Phase 2 — per-user Lark account link.
 *
 * `linked` flips true once the user has completed OAuth at least once.
 * `configured` mirrors the workspace-binding flag so the UI can hide the
 * Connect affordance entirely on deployments missing LARK_APP_ID etc.
 */
export interface LarkUserLinkResponse {
  linked: boolean;
  configured: boolean;
  open_id?: string;
  linked_at?: string;
}

/** Response from POST /api/users/me/lark/link — the URL the browser must navigate to. */
export interface StartLarkUserLinkResponse {
  url: string;
}
