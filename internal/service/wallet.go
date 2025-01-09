package service

import (
	"context"
	"fmt"
	"simo/internal/repository"

	"github.com/rs/zerolog"
)

type WalletService interface {
        AddWalletAlias(ctx context.Context, address, alias string) (*repository.WalletAlias, error)
        RemoveWalletAlias(ctx context.Context, address, alias string) error
        GetWalletAliases(ctx context.Context, address string) ([]repository.WalletAlias, error)
        GetWalletByAlias(ctx context.Context, alias string) (*repository.Wallet, error)
}

type WalletManager struct {
        repo   repository.Repository
        logger zerolog.Logger
}

func NewWalletManager(repo repository.Repository, logger zerolog.Logger) *WalletManager {
        return &WalletManager{
                repo:   repo,
                logger: logger,
        }
}

func (wm *WalletManager) AddWalletAlias(ctx context.Context, address, alias string) (*repository.WalletAlias, error) {
        // Get the wallet first
        wallet, err := wm.repo.GetWalletByAddress(ctx, address)
        if err != nil {
                return nil, fmt.Errorf("failed to get wallet: %w", err)
        }

        // Add the alias
        if err := wm.repo.AddWalletAlias(ctx, wallet.ID, alias); err != nil {
                return nil, fmt.Errorf("failed to add wallet alias: %w", err)
        }

        // Get the aliases to return the newly created one
        aliases, err := wm.repo.GetWalletAliases(ctx, wallet.ID)
        if err != nil {
                return nil, fmt.Errorf("failed to get wallet aliases: %w", err)
        }

        // Find and return the newly created alias
        for _, a := range aliases {
                if a.Alias == alias {
                        return &a, nil
                }
        }

        return nil, fmt.Errorf("alias was added but not found in response")
}

func (wm *WalletManager) RemoveWalletAlias(ctx context.Context, address, alias string) error {
        // Get the wallet first
        wallet, err := wm.repo.GetWalletByAddress(ctx, address)
        if err != nil {
                return fmt.Errorf("failed to get wallet: %w", err)
        }

        // Remove the alias
        if err := wm.repo.RemoveWalletAlias(ctx, wallet.ID, alias); err != nil {
                return fmt.Errorf("failed to remove wallet alias: %w", err)
        }

        return nil
}

func (wm *WalletManager) GetWalletAliases(ctx context.Context, address string) ([]repository.WalletAlias, error) {
        // Get the wallet first
        wallet, err := wm.repo.GetWalletByAddress(ctx, address)
        if err != nil {
                return nil, fmt.Errorf("failed to get wallet: %w", err)
        }

        // Get the aliases
        aliases, err := wm.repo.GetWalletAliases(ctx, wallet.ID)
        if err != nil {
                return nil, fmt.Errorf("failed to get wallet aliases: %w", err)
        }

        return aliases, nil
}

func (wm *WalletManager) GetWalletByAlias(ctx context.Context, alias string) (*repository.Wallet, error) {
        wallet, err := wm.repo.GetWalletByAlias(ctx, alias)
        if err != nil {
                return nil, fmt.Errorf("failed to get wallet by alias: %w", err)
        }

        return wallet, nil
}
