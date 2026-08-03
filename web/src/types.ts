export interface Session {
  csrfToken: string;
  gatewayPrefix: string;
  user: { id: string; username: string };
}

export interface Channel {
  id: 'open' | 'sponsor';
  name: string;
  description: string;
}

export interface Job {
  id: string;
  action: string;
  channel?: string;
  status: 'queued' | 'running' | 'succeeded' | 'failed';
  progress: number;
  message: string;
  error?: string;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
}

export interface Status {
  installed: boolean;
  serviceActive: boolean;
  serviceEnabled: boolean;
  version: string;
  port: number;
  channel: string;
  kvmAvailable: boolean;
  libvirtActive: boolean;
  ovsActive: boolean;
  architecture: string;
  managerVersion: string;
  lastOperation: string;
  databasePresent: boolean;
  networkCompatibility: {
    enabled: boolean;
    mode?: string;
    network?: string;
    bridge?: string;
    repairedVMs?: string[];
    pendingRestart?: string[];
    errors?: string[];
    updatedAt?: string;
  };
  activeJob?: Job;
}

export interface User {
  id: number;
  username: string;
  role: string;
  status: string;
  totpEnabled: boolean;
  email: string;
}
