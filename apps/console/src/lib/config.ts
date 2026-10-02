export function controlPlaneUrl(): string {
  const v =
    process.env.NEXT_PUBLIC_CONTROL_PLANE_URL || "http://localhost:18080";
  return v.replace(/\/$/, "");
}

export function aiServiceUrl(): string {
  const v =
    process.env.NEXT_PUBLIC_AI_SERVICE_URL || "http://localhost:18082";
  return v.replace(/\/$/, "");
}

export function defaultWorkloadId(): string {
  return process.env.NEXT_PUBLIC_WORKLOAD_ID || "";
}

export function defaultMigrationId(): string {
  return process.env.NEXT_PUBLIC_MIGRATION_ID || "";
}

export const POLL_INTERVAL_MS = 5000;
