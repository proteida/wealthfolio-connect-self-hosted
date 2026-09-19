// Package clients wires every concrete BrokerClient (Futu, IBKR, Binance,
// OKX-CEX, OKX-Web3, Bitget, Hyperliquid) into the broker_clients fx group.
// Individual clients live under infrastructure/clients/<name>; this file
// is the composition root.
package clients

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
	"go.uber.org/fx"

	appprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
	steamprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/steamprices"
	appsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/sync"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/binance"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/bitget"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/futu"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/hyperliquid"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/ibkr"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/okx"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/snaptrade"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/steam"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/ton"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/config"
)

// Module registers every BrokerClient into the broker_clients fx group.
//
// Broker connections retain their existing wiring. Crypto integrations are
// flattened into the same group only when their credentials are configured.
// The optional SnapTrade importer joins the same flattened group when enabled.
var Module = fx.Module("infrastructure.clients",
	fx.Provide(
		appsync.AsBrokerClient(NewFutu),
		appsync.AsBrokerClient(NewIBKR),
		NewCryptoClients,
		NewSnapTrade,
		AsSteamPriceProvider(NewSteamPriceProvider),
	),
)

// SnapTradeOut conditionally contributes the enabled SnapTrade client to the
// shared broker_clients group. An empty slice cleanly disables the integration.
type SnapTradeOut struct {
	fx.Out
	Clients []domainsync.BrokerClient `group:"broker_clients,flatten"`
}

// NewSnapTrade builds the optional SnapTrade client from validated config.
func NewSnapTrade(cfg *config.Config, log zerolog.Logger) (SnapTradeOut, error) {
	if cfg == nil || !cfg.SnapTrade.Enabled {
		return SnapTradeOut{}, nil
	}
	client, err := snaptrade.New(cfg.SnapTrade, log.With().Str("client", "snaptrade").Logger(), nil)
	if err != nil {
		return SnapTradeOut{}, err
	}
	return SnapTradeOut{Clients: []domainsync.BrokerClient{client}}, nil
}

// NewFutu builds the Futu BrokerClient from config.
func NewFutu(cfg *config.Config, log zerolog.Logger) *futu.Client {
	var rsaKey []byte
	if cfg.Futu.RSAKeyFile != "" {
		data, err := os.ReadFile(cfg.Futu.RSAKeyFile)
		if err == nil {
			rsaKey = data
		}
	}
	c := futu.New(cfg.Futu.Host, cfg.Futu.Port, cfg.Futu.TradePassword, cfg.Futu.ConnectionID, rsaKey, nil)
	c.SetLogger(log.With().Str("client", "futu").Logger())
	return c
}

// NewIBKR builds the IBKR BrokerClient from config.
func NewIBKR(cfg *config.Config) *ibkr.Client {
	return ibkr.New(cfg.IBKR.Host, cfg.IBKR.Port, cfg.IBKR.ClientID, cfg.IBKR.AccountID, nil)
}

// NewBinance builds the Binance Spot BrokerClient.
func NewBinance(cfg *config.Config, log zerolog.Logger, history repository.ActivityRepository) *binance.Client {
	c := binance.New(cfg.Crypto.BinanceAPIKey, cfg.Crypto.BinanceSecret, nil)
	c.SetLogger(log.With().Str("client", "binance").Logger())
	c.ConfigureHistory(history, cfg.Crypto.BinanceTradeSymbols)
	return c
}

// NewOKXCEX builds the OKX CEX BrokerClient.
func NewOKXCEX(cfg *config.Config, history repository.ActivityRepository, cursors repository.CursorRepository) *okx.CEXClient {
	c := okx.NewCEX(okx.Credentials{
		APIKey:     cfg.Crypto.OKXAPIKey,
		Secret:     cfg.Crypto.OKXSecret,
		Passphrase: cfg.Crypto.OKXPassphrase,
	}, "", nil)
	c.ConfigureHistory(history)
	c.ConfigureCursors(cursors)
	return c
}

// NewOKXWeb3 builds the OKX Web3 BrokerClient that fans out across every
// configured wallet. The structured logger is injected so per-wallet fetch
// failures (typically transient OKX outages or chain RPC blips) surface in
// the application log instead of being silently dropped.
func NewOKXWeb3(cfg *config.Config, log zerolog.Logger) *okx.Web3Client {
	wallets := make([]okx.Wallet, 0, len(cfg.DefiWallets))
	for _, w := range cfg.DefiWallets {
		wallets = append(wallets, okx.Wallet{
			Address: w.Address,
			Chains:  w.Chains,
			Label:   w.Name,
		})
	}
	c := okx.NewWeb3(okx.Credentials{
		APIKey:     cfg.Crypto.OKXWeb3APIKey,
		Secret:     cfg.Crypto.OKXWeb3Secret,
		Passphrase: cfg.Crypto.OKXWeb3Passphrase,
	}, wallets, "", nil)
	c.SetLogger(log.With().Str("component", "okx_web3").Logger())
	return c
}

// NewBitget builds the Bitget Spot BrokerClient.
func NewBitget(cfg *config.Config) *bitget.Client {
	return bitget.New(
		cfg.Crypto.BitgetAPIKey,
		cfg.Crypto.BitgetSecret,
		cfg.Crypto.BitgetPassphrase,
		"", nil,
	)
}

// NewHyperliquid builds the Hyperliquid BrokerClient.
func NewHyperliquid(cfg *config.Config, prices *appprices.Service) *hyperliquid.Client {
	c := hyperliquid.New(cfg.Crypto.HyperliquidWallet, "", nil)
	if prices != nil {
		c.SetPriceService(prices)
	}
	return c
}

// NewTON builds the TON Center BrokerClient tracking the configured wallets.
func NewTON(cfg *config.Config, log zerolog.Logger, cursors repository.CursorRepository, history repository.ActivityRepository, priceHistory repository.PriceHistoryRepository) *ton.Client {
	c := ton.New(cfg.Crypto.TONCenterAPIKey, cfg.Crypto.TONWallets, "", nil)
	c.SetLogger(log.With().Str("client", "ton").Logger())
	c.ConfigureCursors(cursors)
	c.ConfigureHistory(history)
	if priceHistory != nil {
		c.SetPriceHistory(priceHistory)
	}
	return c
}

// NewSteam builds the Steam CS2 inventory BrokerClient.
func NewSteam(cfg *config.Config, log zerolog.Logger, history repository.ActivityRepository, cursors repository.CursorRepository, prices *appprices.Service, store repository.SteamAssetRepository, priceHistory repository.PriceHistoryRepository) *steam.Client {
	c := steam.New(steam.ClientConfig{
		SteamID:         cfg.Steam.SteamID,
		APIKey:          cfg.Steam.APIKey,
		Session:         cfg.Steam.Session,
		RefreshToken:    cfg.Steam.RefreshToken,
		PriceTTL:        cfg.Steam.PriceTTL,
		Currency:        cfg.Steam.Currency,
		HistoryBudget:   cfg.Steam.HistoryBudget,
		MinItemValueUSD: cfg.Steam.MinItemValueUSD,
	}, nil)
	c.SetLogger(log.With().Str("client", "steam").Logger())
	c.ConfigureHistory(history)
	c.ConfigureCursors(cursors)
	if prices != nil {
		c.SetPriceService(prices)
	}
	if store != nil {
		c.SetSteamStore(store)
	}
	if priceHistory != nil {
		c.SetPriceHistoryStore(priceHistory)
	}
	return c
}

// AsSteamPriceProvider annotates a Steam price provider for fx.
func AsSteamPriceProvider(f any) any {
	return fx.Annotate(f, fx.As(new(steamprices.Provider)))
}

// NewSteamPriceProvider builds the public Steam price provider. The
// priceoverview endpoint needs no credentials, so none are configured:
// public reads must never trigger session refresh flows or attach
// cookies. Fetched current quotes are remembered through the shared
// history store for 24h under a dedicated cache namespace so they can
// never satisfy the sync client's shorter freshness window.
func NewSteamPriceProvider(cfg *config.Config, prices *appprices.Service, history repository.PriceHistoryRepository) *steam.Client {
	c := steam.New(steam.ClientConfig{
		SteamID:         cfg.Steam.SteamID,
		PriceTTL:        steamprices.PublicPriceTTL,
		Currency:        cfg.Steam.Currency,
		HistoryBudget:   0,
		MinItemValueUSD: 0,
		CacheNamespace:  "public",
	}, nil)
	if prices != nil {
		c.SetPriceService(prices)
	}
	c.SetPriceHistoryStore(history)
	return c
}

// CryptoClients is the flattened fx group of configured crypto integrations.
type CryptoClients struct {
	fx.Out
	Clients []domainsync.BrokerClient `group:"broker_clients,flatten"`
}

// NewCryptoClients registers only integrations whose required settings exist.
// Partial credentials produce one startup warning, never recurring API calls.
func NewCryptoClients(cfg *config.Config, log zerolog.Logger, history repository.ActivityRepository, cursors repository.CursorRepository, priceHistory repository.PriceHistoryRepository, prices *appprices.Service, steamStore repository.SteamAssetRepository) CryptoClients {
	out := CryptoClients{}
	enabled := func(name string, fields ...string) bool {
		count := 0
		for _, field := range fields {
			if strings.TrimSpace(field) != "" {
				count++
			}
		}
		if count == len(fields) {
			return true
		}
		if count > 0 {
			log.Warn().Str("client", name).Msg("integration disabled: incomplete configuration")
		}
		return false
	}
	if enabled("binance", cfg.Crypto.BinanceAPIKey, cfg.Crypto.BinanceSecret) {
		out.Clients = append(out.Clients, NewBinance(cfg, log, history))
	}
	if enabled("okx", cfg.Crypto.OKXAPIKey, cfg.Crypto.OKXSecret, cfg.Crypto.OKXPassphrase) {
		out.Clients = append(out.Clients, NewOKXCEX(cfg, history, cursors))
	}
	if enabled("bitget", cfg.Crypto.BitgetAPIKey, cfg.Crypto.BitgetSecret, cfg.Crypto.BitgetPassphrase) {
		out.Clients = append(out.Clients, NewBitget(cfg))
	}
	if enabled("hyperliquid", cfg.Crypto.HyperliquidWallet) {
		out.Clients = append(out.Clients, NewHyperliquid(cfg, prices))
	}
	if len(cfg.DefiWallets) > 0 && enabled("okx_web3", cfg.Crypto.OKXWeb3APIKey, cfg.Crypto.OKXWeb3Secret, cfg.Crypto.OKXWeb3Passphrase) {
		out.Clients = append(out.Clients, NewOKXWeb3(cfg, log))
	}
	if len(cfg.Crypto.TONWallets) > 0 {
		if strings.TrimSpace(cfg.Crypto.TONCenterAPIKey) == "" {
			log.Warn().Msg("ton enabled without TONCENTER_API_KEY: 1 RPS anonymous quota applies")
		}
		out.Clients = append(out.Clients, NewTON(cfg, log, cursors, history, priceHistory))
	}
	if strings.TrimSpace(cfg.Steam.SteamID) != "" {
		if strings.TrimSpace(cfg.Steam.Session) == "" && strings.TrimSpace(cfg.Steam.RefreshToken) == "" {
			log.Warn().Msg("steam enabled without STEAM_SESSION or STEAM_REFRESH_TOKEN: only public inventory and prices are available")
		}
		out.Clients = append(out.Clients, NewSteam(cfg, log, history, cursors, prices, steamStore, priceHistory))
	}
	return out
}
