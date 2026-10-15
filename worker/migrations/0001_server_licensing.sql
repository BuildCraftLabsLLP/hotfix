-- Hotfix Server licensing (worker/src/server-api.js). D1 (SQLite) rather than
-- KV because seat counting must be atomic: KV is eventually consistent and
-- would oversell seats when a fleet activates at once.

-- A license = one activation key (deployed to servers) + one manage key
-- (billing). Only SHA-256 hashes of the keys are stored.
CREATE TABLE licenses (
  id              TEXT PRIMARY KEY,           -- lic_<random>
  activation_hash TEXT NOT NULL UNIQUE,
  manage_hash     TEXT NOT NULL UNIQUE,
  created         INTEGER NOT NULL
);

-- Razorpay subscriptions. Seats = sum of quantity over a license's live
-- subscriptions. A license can have several (adding servers to a UPI or
-- e-mandate subscription means buying a second one, since those can't be
-- updated).
CREATE TABLE subscriptions (
  id                 TEXT PRIMARY KEY,        -- sub_… (Razorpay)
  license_id         TEXT,                    -- set once paid
  pending_license_id TEXT,                    -- "add servers to this license" checkouts
  status             TEXT NOT NULL,           -- Razorpay status
  quantity           INTEGER NOT NULL,
  current_end        INTEGER,                 -- end of the paid period (unix)
  cancel_scheduled   INTEGER NOT NULL DEFAULT 0,
  created            INTEGER NOT NULL,
  updated            INTEGER NOT NULL
);
CREATE INDEX subscriptions_license ON subscriptions (license_id);

-- A seat is a lease held by one server (instance_id), renewed by every
-- license refresh; an expired lease frees the seat by itself.
CREATE TABLE seats (
  id          TEXT PRIMARY KEY,               -- seat_<random>
  license_id  TEXT NOT NULL,
  secret_hash TEXT NOT NULL,                  -- the server refreshes with seat id + secret
  fingerprint TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  hostname    TEXT,
  os          TEXT,
  version     TEXT,
  boot_id     TEXT,
  created     INTEGER NOT NULL,
  last_seen   INTEGER NOT NULL,
  lease_until INTEGER NOT NULL
);
CREATE UNIQUE INDEX seats_instance ON seats (license_id, instance_id);
CREATE INDEX seats_lease ON seats (license_id, lease_until);
