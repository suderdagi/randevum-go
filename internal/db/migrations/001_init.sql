-- Needed for the exclusion constraint that prevents double bookings
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TYPE user_role AS ENUM ('customer', 'owner');
CREATE TYPE appointment_status AS ENUM ('pending', 'confirmed', 'cancelled', 'completed');

CREATE TABLE users (
  id            SERIAL PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE CHECK (email = lower(email)),
  password_hash TEXT NOT NULL,
  full_name     TEXT NOT NULL,
  phone         TEXT,
  role          user_role NOT NULL DEFAULT 'customer',
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only a SHA-256 hash of each refresh token is stored, never the token itself
CREATE TABLE refresh_tokens (
  id          SERIAL PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash  TEXT NOT NULL UNIQUE,
  expires_at  TIMESTAMPTZ NOT NULL,
  revoked_at  TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX refresh_tokens_user_idx ON refresh_tokens (user_id);

CREATE TABLE businesses (
  id          SERIAL PRIMARY KEY,
  owner_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  slug        TEXT NOT NULL UNIQUE,
  category    TEXT NOT NULL,
  city        TEXT NOT NULL,
  address     TEXT,
  phone       TEXT,
  description TEXT,
  timezone    TEXT NOT NULL DEFAULT 'Europe/Istanbul',
  -- Customers can cancel up to this many hours before the appointment
  cancellation_hours INTEGER NOT NULL DEFAULT 2 CHECK (cancellation_hours >= 0),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX businesses_owner_idx ON businesses (owner_id);
CREATE INDEX businesses_city_category_idx ON businesses (city, category);

-- weekday follows ISO 8601: 1 = Monday ... 7 = Sunday. A missing row means closed.
CREATE TABLE working_hours (
  business_id INTEGER NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  weekday     SMALLINT NOT NULL CHECK (weekday BETWEEN 1 AND 7),
  opens_at    TIME NOT NULL,
  closes_at   TIME NOT NULL,
  PRIMARY KEY (business_id, weekday),
  CHECK (opens_at < closes_at)
);

CREATE TABLE services (
  id               SERIAL PRIMARY KEY,
  business_id      INTEGER NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  name             TEXT NOT NULL,
  duration_minutes INTEGER NOT NULL CHECK (duration_minutes BETWEEN 5 AND 480),
  price            NUMERIC(10, 2) NOT NULL CHECK (price >= 0),
  is_active        BOOLEAN NOT NULL DEFAULT true,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX services_business_idx ON services (business_id);

CREATE TABLE appointments (
  id           SERIAL PRIMARY KEY,
  business_id  INTEGER NOT NULL REFERENCES businesses(id) ON DELETE CASCADE,
  service_id   INTEGER NOT NULL REFERENCES services(id),
  customer_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  starts_at    TIMESTAMPTZ NOT NULL,
  ends_at      TIMESTAMPTZ NOT NULL,
  status       appointment_status NOT NULL DEFAULT 'pending',
  note         TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (ends_at > starts_at),

  -- Two active appointments of the same business can never overlap.
  -- The database enforces this, so two customers booking the same slot
  -- at the same moment cannot both succeed (no race condition).
  CONSTRAINT appointments_no_overlap EXCLUDE USING gist (
    business_id WITH =,
    tstzrange(starts_at, ends_at) WITH &&
  ) WHERE (status IN ('pending', 'confirmed'))
);
CREATE INDEX appointments_customer_idx ON appointments (customer_id, starts_at DESC);
CREATE INDEX appointments_business_time_idx ON appointments (business_id, starts_at);
