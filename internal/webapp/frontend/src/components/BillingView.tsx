import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import type { BillingInfo } from "../api/types";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";

// The Billing view (managed hubs) renders inside the app shell; the data and
// the money actions live on the hub's billing endpoints — the SPA only knows
// the URL /api/config handed it. Checkout and the portal are plain form
// POSTs: both leave the app for Stripe anyway, so a full-page navigation is
// the honest shape.
export function BillingView({ url }: { url: string }) {
  const q = useQuery({ queryKey: ["billing"], queryFn: () => getJSON<BillingInfo>(url) });
  if (q.isLoading) return <div className="empty">Loading…</div>;
  if (q.error || !q.data) {
    return (
      <div className="empty">
        <h3>Billing is unavailable</h3>
        <p>{(q.error as Error | null)?.message || "Try again shortly."}</p>
      </div>
    );
  }
  const b = q.data;
  return (
    <div className="project-settings" id="billing-view">
      <h2>
        Billing
        <span className="ps-chip plan-chip">{b.plan.name}</span>
      </h2>

      <Card>
        <CardHeader>
          <CardTitle>
            {b.plan.name} plan{b.plan.status ? ` (${b.plan.status})` : ""}
          </CardTitle>
          <CardDescription>
            Organization {b.org} · {b.usage.used} of {b.usage.cap} used · {b.seats.used} of {b.seats.cap}{" "}
            {b.seats.cap === 1 ? "seat" : "seats"}
          </CardDescription>
        </CardHeader>
        <Separator />
        <CardContent>
          <div className="usage-bar">
            <div style={{ width: `${b.usage.pct}%` }} />
          </div>
        </CardContent>
      </Card>

      {b.owner ? (
        <div className="plan-grid">
          {b.plans.map((p) => (
            <Card key={p.id}>
              <CardHeader>
                <CardTitle>{p.name}</CardTitle>
                <CardDescription>{p.blurb}</CardDescription>
              </CardHeader>
              <Separator />
              <CardContent>
                <p className="plan-price">
                  {p.price}
                  <small> / user / month</small>
                </p>
                <form method="post" action={b.checkout_url}>
                  <input type="hidden" name="plan" value={p.id} />
                  <Button type="submit" disabled={p.current} variant={p.current ? "subtle" : "default"}>
                    {p.current ? "Current plan" : `Upgrade to ${p.name}`}
                  </Button>
                </form>
              </CardContent>
            </Card>
          ))}
        </div>
      ) : (
        <p className="muted-note">Only an organization owner can change the plan.</p>
      )}

      {b.owner && b.has_customer && (
        <Card>
          <CardHeader>
            <CardTitle>Manage subscription</CardTitle>
            <CardDescription>Change seats, update the card, download invoices, or cancel.</CardDescription>
          </CardHeader>
          <Separator />
          <CardContent>
            <form method="post" action={b.portal_url}>
              <Button type="submit" variant="subtle">
                Open the billing portal
              </Button>
            </form>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
