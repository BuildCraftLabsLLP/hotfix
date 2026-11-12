-- Billing moved from Razorpay to Dodo Payments (src/app.js). subscriptions.id
-- and .status now hold Dodo's subscription id (sub_…) and status.

-- A Dodo subscription exists only once its checkout is paid, so each checkout
-- is recorded first and matched to its subscription afterwards.
CREATE TABLE checkouts (
  ref                TEXT PRIMARY KEY,        -- random; in the return URL and the session metadata
  session_id         TEXT NOT NULL UNIQUE,    -- cks_… (Dodo checkout session)
  pending_license_id TEXT REFERENCES licenses (id),   -- "add servers to this license" checkouts
  quantity           INTEGER NOT NULL,
  subscription_id    TEXT,                    -- sub_… once paid and matched
  created            BIGINT NOT NULL
);

-- The Dodo customer, for their billing portal (invoices, payment method).
ALTER TABLE subscriptions ADD COLUMN customer_id TEXT;
