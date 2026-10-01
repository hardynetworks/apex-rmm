// Bridge to the Apex RMM Windows desktop app (desktop/). In a normal browser `native` is undefined.

export type ApexNative = {
  platform: 'windows';
  version: string;
  kind: 'main' | 'session';
  openSession(url: string): Promise<boolean>;
  openExternal(url: string): Promise<boolean>;
  notify(title: string, body: string, link?: string): Promise<boolean>;
  /** Ask the app to capture Windows shortcuts (Win, Alt+Tab, …) for this window. Resolves to the user's setting. */
  setCapture(on: boolean): Promise<boolean>;
  setCaptureDefault(on: boolean): Promise<boolean>;
  getConfig(): Promise<{ server: string; version: string; captureKeys: boolean; autostart: boolean }>;
  closeWindow(): Promise<void>;
};

export const native: ApexNative | undefined = (window as any).apexNative;
