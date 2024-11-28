-- Wallets table to store addresses we're tracking
CREATE TABLE wallets (
    id SERIAL PRIMARY KEY,
    address VARCHAR(44) NOT NULL UNIQUE,  -- Solana addresses are 44 characters
    created_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW()
);

-- Token positions table to track current holdings
CREATE TABLE token_positions (
    id SERIAL PRIMARY KEY,
    wallet_id INTEGER REFERENCES wallets(id),
    token_address VARCHAR(44) NOT NULL,
    token_name VARCHAR(100),
    current_amount DECIMAL(20, 8) NOT NULL,  -- Current holding amount
    average_entry_price DECIMAL(20, 8),
    first_purchase_at TIMESTAMP WITH TIME ZONE,
    last_updated_at TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
    UNIQUE(wallet_id, token_address)
);

-- Transactions table to track all trades
CREATE TABLE transactions (
    id SERIAL PRIMARY KEY,
    wallet_id INTEGER REFERENCES wallets(id),
    token_in VARCHAR(44) NOT NULL,
    token_out VARCHAR(44) NOT NULL,
    amount_in DECIMAL NOT NULL,
    amount_out DECIMAL NOT NULL,
    program VARCHAR(50) NOT NULL,
    signature VARCHAR(100) UNIQUE NOT NULL,
    timestamp TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Create indexes for better query performance
CREATE INDEX idx_wallets_address ON wallets(address);
CREATE INDEX idx_transactions_wallet_token ON transactions(wallet_id, token_in);
CREATE INDEX idx_token_positions_wallet ON token_positions(wallet_id);
