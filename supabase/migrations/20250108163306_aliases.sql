-- Create wallet_aliases table
CREATE TABLE IF NOT EXISTS wallet_aliases (
    id BIGSERIAL PRIMARY KEY,
    wallet_id BIGINT NOT NULL REFERENCES wallets(id) ON DELETE CASCADE,
    alias VARCHAR(255) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(wallet_id, alias)
);

-- Create an index on alias for faster lookups
CREATE INDEX idx_wallet_aliases_alias ON wallet_aliases(alias);

-- Create updated_at trigger
CREATE OR REPLACE FUNCTION trigger_set_timestamp()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at = NOW();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER set_timestamp
    BEFORE UPDATE ON wallet_aliases
    FOR EACH ROW
    EXECUTE FUNCTION trigger_set_timestamp();
