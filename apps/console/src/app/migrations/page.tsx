"use client";

import { Suspense, useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { ErrorState, Loading } from "@/components/cards";
import { apiMigrationsForWorkload, apiWorkloads } from "@/lib/api";

function Redirector() {
  const router = useRouter();
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    (async () => {
      const params = new URLSearchParams(window.location.search);
      const widHint = params.get("workload");
      let wid = widHint;
      if (!wid) {
        const wl = await apiWorkloads();
        if (wl.error || !wl.data || (wl.data.items || []).length === 0) {
          setError(wl.error || "no workloads registered");
          return;
        }
        wid = (wl.data.items[0] as Record<string, unknown>).id as string;
      }
      const migs = await apiMigrationsForWorkload(wid!);
      if (migs.error || !migs.data || (migs.data.items || []).length === 0) {
        setError(migs.error || "no migrations for workload");
        return;
      }
      const mid = (migs.data.items[0] as Record<string, unknown>).id as string;
      router.replace(`/migrations/${mid}?workload=${wid}`);
    })();
  }, [router]);

  if (error) return <ErrorState label="Migration" error={error} />;
  return <Loading label="migration" />;
}

export default function MigrationsIndex() {
  return (
    <Suspense fallback={<Loading label="migration" />}>
      <Redirector />
    </Suspense>
  );
}
