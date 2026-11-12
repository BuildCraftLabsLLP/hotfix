-- Refund requests from the manage page (src/app.js requestRefund): one per
-- payment, emailed to us. Refunding the payment in Dodo's dashboard ends the
-- subscription (refund.succeeded webhook).
CREATE TABLE refund_requests (
  id              TEXT PRIMARY KEY,           -- rr_<random>
  license_id      TEXT NOT NULL REFERENCES licenses (id),
  subscription_id TEXT NOT NULL REFERENCES subscriptions (id),
  payment_id      TEXT NOT NULL UNIQUE,       -- pay_… (Dodo)
  reason          TEXT NOT NULL,              -- the buyer's words, maybe empty
  created         BIGINT NOT NULL
);
