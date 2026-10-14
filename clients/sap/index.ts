/**
 * Sandbox Access Protocol v1alpha1 client.
 * Uses the global fetch and WebSocket implementations. No package dependencies.
 */

export const apiVersion = "sandboxaccess.agents.x-k8s.io/v1alpha1";
export const subprotocol = "sap.v1";

export type Phase = "Pending" | "Ready" | "Paused" | "Blocked" | "Terminating";
export type Activity = "idle" | "working" | "waiting" | "blocked";
export type Channel = "terminal" | "control";

export interface Sandbox {
  apiVersion: string;
  kind: "Sandbox";
  id: string;
  namespace: string;
  name: string;
  labels?: Record<string, string>;
  phase: Phase;
  activity: Activity;
  lastActivity?: string;
  project?: string;
  owner?: string;
  channels: Channel[];
}

export interface SandboxList {
  apiVersion: string;
  kind: "SandboxList";
  items: Sandbox[];
}

export interface Frame {
  type: "input" | "output" | "resize" | "status" | "exit" | "error" | "close";
  data?: string;
  stream?: "stdout";
  cols?: number;
  rows?: number;
  code?: number;
  activity?: Activity;
  lastActivity?: string;
  message?: string;
  channel?: Channel;
}

export class StatusError extends Error {
  constructor(readonly status: number, message: string) {
    super(message);
  }
}

export class Client {
  constructor(
    private readonly baseURL: string,
    private readonly token: string,
  ) {}

  list(): Promise<SandboxList> {
    return this.get("/sap/v1/sandboxes");
  }

  getSandbox(namespace: string, name: string): Promise<Sandbox> {
    return this.get(`/sap/v1/sandboxes/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`);
  }

  attach(namespace: string, name: string, channel: Channel = "terminal"): WebSocket {
    const url = new URL(this.baseURL);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    url.pathname = `${url.pathname.replace(/\/$/, "")}/sap/v1/sandboxes/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}/attach`;
    url.search = channel === "terminal" ? "" : `channel=${encodeURIComponent(channel)}`;
    return new WebSocket(url, [subprotocol]);
  }

  private async get<T>(path: string): Promise<T> {
    const response = await fetch(`${this.baseURL.replace(/\/$/, "")}${path}`, {
      headers: { authorization: `Bearer ${this.token}` },
    });
    if (!response.ok) {
      const body = (await response.json().catch(() => ({}))) as { error?: string };
      throw new StatusError(response.status, body.error ?? response.statusText);
    }
    return (await response.json()) as T;
  }
}

export function inputFrame(bytes: Uint8Array): string {
  return JSON.stringify({ type: "input", data: bytesToBase64(bytes) });
}

export function outputBytes(frame: Frame): Uint8Array {
  if (!frame.data) return new Uint8Array();
  return base64ToBytes(frame.data);
}

function bytesToBase64(bytes: Uint8Array): string {
  let text = "";
  for (const byte of bytes) text += String.fromCharCode(byte);
  return btoa(text);
}

function base64ToBytes(value: string): Uint8Array {
  const text = atob(value);
  const bytes = new Uint8Array(text.length);
  for (let i = 0; i < text.length; i++) bytes[i] = text.charCodeAt(i);
  return bytes;
}
