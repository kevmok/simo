package repository

import (
	"context"
	"errors"
	"time"

	"github.com/georgysavva/scany/v2/pgxscan"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrWalletNotFound      = errors.New("wallet not found")
	ErrTokenPositionEmpty  = errors.New("token position is empty")
	ErrTransactionNotFound = errors.New("transaction not found")
)

type Wallet struct {
	ID        int       `db:"id"`
	Address   string    `db:"address"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

type TokenPosition struct {
	ID              int       `db:"id"`
	WalletID        int       `db:"wallet_id"`
	TokenAddress    string    `db:"token_address"`
	TokenName       string    `db:"token_name"`
	CurrentAmount   float64   `db:"current_amount"`
	AvgEntryPrice   float64   `db:"average_entry_price"`
	FirstPurchaseAt time.Time `db:"first_purchase_at"`
	LastUpdatedAt   time.Time `db:"last_updated_at"`
}

type Transaction struct {
	ID        int       `db:"id"`
	WalletID  int       `db:"wallet_id"`
	TokenIn   string    `db:"token_in"`
	TokenOut  string    `db:"token_out"`
	AmountIn  float64   `db:"amount_in"`
	AmountOut float64   `db:"amount_out"`
	Program   string    `db:"program"` // DEX used
	Signature string    `db:"signature"`
	Timestamp time.Time `db:"timestamp"`
}

type Repository interface {
	// Wallet operations
	AddWallet(ctx context.Context, address string) error
	RemoveWallet(ctx context.Context, address string) error
	GetWallets(ctx context.Context) ([]Wallet, error)
	GetWalletByAddress(ctx context.Context, address string) (*Wallet, error)

	// Token position operations
	UpdateTokenPosition(ctx context.Context, pos *TokenPosition) error
	GetTokenPositions(ctx context.Context, walletID int) ([]TokenPosition, error)
	RemoveTokenPosition(ctx context.Context, walletID int, tokenAddress string) error

	// Transaction operations
	AddTransaction(ctx context.Context, tx *Transaction) error
	GetTransactions(ctx context.Context, walletID int, tokenAddress string) ([]Transaction, error)
}

type PostgresRepository struct {
	db *pgxpool.Pool
}

type ProfitLoss struct {
	TokenAddress    string
	RealizedPL      float64
	UnrealizedPL    float64
	TotalPL         float64
	RemainingAmount float64
	AverageEntry    float64
	LastPrice       float64
}

func NewRepository(db *pgxpool.Pool) Repository {
	return &PostgresRepository{db: db}
}

func (r *PostgresRepository) AddWallet(ctx context.Context, address string) error {
	query := `
		INSERT INTO wallets (address)
		VALUES ($1)
		ON CONFLICT (address) DO NOTHING
		RETURNING id`

	var id int
	err := pgxscan.Get(ctx, r.db, &id, query, address)
	if err != nil {
		return err
	}
	return nil
}

func (r *PostgresRepository) UpdateTokenPosition(ctx context.Context, pos *TokenPosition) error {
	query := `
		INSERT INTO token_positions (
			wallet_id, token_address, token_name, current_amount,
			average_entry_price, first_purchase_at, last_updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (wallet_id, token_address) DO UPDATE SET
			current_amount = $4,
			average_entry_price = $5,
			last_updated_at = NOW()`

	_, err := r.db.Exec(ctx, query,
		pos.WalletID,
		pos.TokenAddress,
		pos.TokenName,
		pos.CurrentAmount,
		pos.AvgEntryPrice,
		pos.FirstPurchaseAt,
	)
	return err
}

func (r *PostgresRepository) AddTransaction(ctx context.Context, tx *Transaction) error {
	query := `
        INSERT INTO transactions (
            wallet_id, token_in, token_out,
            amount_in, amount_out, program, signature, timestamp
        ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
        ON CONFLICT (signature) DO NOTHING`

	_, err := r.db.Exec(ctx, query,
		tx.WalletID,
		tx.TokenIn,
		tx.TokenOut,
		tx.AmountIn,
		tx.AmountOut,
		tx.Program,
		tx.Signature,
		tx.Timestamp,
	)
	return err
}

func (r *PostgresRepository) RemoveWallet(ctx context.Context, address string) error {
	query := `DELETE FROM wallets WHERE address = $1`
	result, err := r.db.Exec(ctx, query, address)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrWalletNotFound
	}
	return nil
}

func (r *PostgresRepository) GetWallets(ctx context.Context) ([]Wallet, error) {
	var wallets []Wallet
	query := `SELECT * FROM wallets ORDER BY created_at DESC`
	if err := pgxscan.Select(ctx, r.db, &wallets, query); err != nil {
		return nil, err
	}
	return wallets, nil
}

func (r *PostgresRepository) GetWalletByAddress(ctx context.Context, address string) (*Wallet, error) {
	var wallet Wallet
	query := `SELECT * FROM wallets WHERE address = $1`
	if err := pgxscan.Get(ctx, r.db, &wallet, query, address); err != nil {
		return nil, ErrWalletNotFound
	}
	return &wallet, nil
}

// Token position operations
func (r *PostgresRepository) GetTokenPositions(ctx context.Context, walletID int) ([]TokenPosition, error) {
	var positions []TokenPosition
	query := `
		SELECT * FROM token_positions
		WHERE wallet_id = $1 AND current_amount > 0
		ORDER BY last_updated_at DESC`

	if err := pgxscan.Select(ctx, r.db, &positions, query, walletID); err != nil {
		return nil, err
	}
	return positions, nil
}

func (r *PostgresRepository) RemoveTokenPosition(ctx context.Context, walletID int, tokenAddress string) error {
	query := `DELETE FROM token_positions WHERE wallet_id = $1 AND token_address = $2`
	result, err := r.db.Exec(ctx, query, walletID, tokenAddress)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return ErrTokenPositionEmpty
	}
	return nil
}

// Transaction operations
func (r *PostgresRepository) GetTransactions(ctx context.Context, walletID int, tokenAddress string) ([]Transaction, error) {
	var transactions []Transaction
	query := `
		SELECT * FROM transactions
		WHERE wallet_id = $1 AND token_address = $2
		ORDER BY timestamp DESC`

	if err := pgxscan.Select(ctx, r.db, &transactions, query, walletID, tokenAddress); err != nil {
		return nil, err
	}
	return transactions, nil
}
