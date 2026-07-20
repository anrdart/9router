import { Skeleton } from "@/shared/components/Loading";

export default function DashboardLoading() {
  return (
    <div role="status" aria-label="Loading dashboard" className="flex flex-col gap-6">
      <span className="sr-only">Loading dashboard</span>
      <div aria-hidden="true" className="space-y-2">
        <Skeleton className="h-7 w-48" />
        <Skeleton className="h-4 w-64 max-w-full" />
      </div>
      <div aria-hidden="true" className="space-y-6">
        <div className="rounded-[14px] border border-border-subtle bg-surface p-6 shadow-[var(--shadow-soft)]">
          <Skeleton className="mb-6 h-6 w-40" />
          <div className="space-y-4">
            <Skeleton className="h-11 w-full" />
            <Skeleton className="h-11 w-full" />
            <Skeleton className="h-11 w-full" />
          </div>
        </div>
        <div className="rounded-[14px] border border-border-subtle bg-surface p-6 shadow-[var(--shadow-soft)]">
          <div className="mb-6 flex items-center justify-between gap-4">
            <Skeleton className="h-6 w-32" />
            <Skeleton className="h-10 w-28" />
          </div>
          <Skeleton className="h-24 w-full" />
        </div>
      </div>
    </div>
  );
}
