package clients_test

import (
	"bytes"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rs/zerolog"
	"go.uber.org/fx"
	"go.uber.org/mock/gomock"

	appprices "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/application/prices"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository"
	repomocks "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/repository/mocks"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/config"
)

func TestClientsModule(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Clients Module Suite")
}

// sampleCfg builds a config with every integration's credentials populated
// just enough to drive the constructors. Real network calls are never made
// from these tests — they only verify the wiring is consistent.
func sampleCfg() *config.Config {
	return &config.Config{
		Futu: config.FutuConfig{
			Host: "127.0.0.1", Port: 11111,
			TradePassword: "secret", ConnectionID: "wftest",
		},
		IBKR: config.IBKRConfig{
			Host: "127.0.0.1", Port: 4002, ClientID: 17,
		},
		Crypto: config.CryptoConfig{
			BinanceAPIKey: "bk", BinanceSecret: "bs",
			OKXAPIKey: "ok", OKXSecret: "os", OKXPassphrase: "op",
			BitgetAPIKey: "gk", BitgetSecret: "gs", BitgetPassphrase: "gp",
			HyperliquidWallet: "0x1111",
			OKXWeb3APIKey:     "wk", OKXWeb3Secret: "ws", OKXWeb3Passphrase: "wp",
		},
		DefiWallets: []config.DefiWallet{
			{Name: "Main", Address: "0xabc", Chains: []string{"1", "42161"}},
			{Name: "Cold", Address: "0xdef", Chains: []string{"137"}},
		},
	}
}

var _ = Describe("Client constructors", func() {
	cfg := sampleCfg()

	It("Futu client returns slug futu", func() {
		Expect(clients.NewFutu(cfg, zerolog.Nop()).ID()).To(Equal("futu"))
	})
	It("IBKR client returns slug ibkr", func() {
		Expect(clients.NewIBKR(cfg).ID()).To(Equal("ibkr"))
	})
	It("OKX CEX client returns slug okx", func() {
		Expect(clients.NewOKXCEX(cfg, nil, nil).ID()).To(Equal("okx"))
	})
	It("Binance client returns slug binance", func() {
		Expect(clients.NewBinance(cfg, zerolog.Nop(), nil).ID()).To(Equal("binance"))
	})
	It("Bitget client returns slug bitget", func() {
		Expect(clients.NewBitget(cfg).ID()).To(Equal("bitget"))
	})
	It("Hyperliquid client returns slug hyperliquid", func() {
		Expect(clients.NewHyperliquid(cfg, nil).ID()).To(Equal("hyperliquid"))
	})
	It("OKX Web3 client returns slug okx_web3", func() {
		Expect(clients.NewOKXWeb3(cfg, zerolog.Nop()).ID()).To(Equal("okx_web3"))
	})
	It("TON client returns slug ton", func() {
		Expect(clients.NewTON(cfg, zerolog.Nop(), nil, nil, nil).ID()).To(Equal("ton"))
	})
	It("Module is non-nil", func() {
		Expect(clients.Module).NotTo(BeNil())
	})
})

var _ = Describe("Configured crypto registration", func() {
	clientIDs := func(out clients.CryptoClients) []string {
		ids := make([]string, 0, len(out.Clients))
		for _, c := range out.Clients {
			ids = append(ids, c.ID())
		}
		return ids
	}
	It("disables all unconfigured crypto integrations", func() {
		Expect(clients.NewCryptoClients(&config.Config{}, zerolog.Nop(), nil, nil, nil, nil, nil).Clients).To(BeEmpty())
	})
	It("registers Binance and Web3 independently of empty OKX CEX and Bitget credentials", func() {
		cfg := sampleCfg()
		cfg.Crypto.OKXAPIKey = ""
		cfg.Crypto.OKXSecret = ""
		cfg.Crypto.OKXPassphrase = ""
		cfg.Crypto.BitgetAPIKey = ""
		cfg.Crypto.BitgetSecret = ""
		cfg.Crypto.BitgetPassphrase = ""
		cfg.Crypto.HyperliquidWallet = ""
		out := clients.NewCryptoClients(cfg, zerolog.Nop(), nil, nil, nil, nil, nil)
		ids := []string{}
		for _, c := range out.Clients {
			ids = append(ids, c.ID())
		}
		Expect(ids).To(ConsistOf("binance", "okx_web3"))
	})
	It("warns once at construction for partial credentials and excludes that client", func() {
		var logs bytes.Buffer
		cfg := &config.Config{Crypto: config.CryptoConfig{OKXAPIKey: "k", BitgetAPIKey: "k"}}
		Expect(clients.NewCryptoClients(cfg, zerolog.New(&logs), nil, nil, nil, nil, nil).Clients).To(BeEmpty())
		Expect(logs.String()).To(ContainSubstring("integration disabled: incomplete configuration"))
	})
	It("registers TON only when wallets are configured", func() {
		cfg := sampleCfg()
		Expect(clientIDs(clients.NewCryptoClients(cfg, zerolog.Nop(), nil, nil, nil, nil, nil))).NotTo(ContainElement("ton"))
		cfg.Crypto.TONWallets = []string{"UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
		Expect(clientIDs(clients.NewCryptoClients(cfg, zerolog.Nop(), nil, nil, nil, nil, nil))).To(ContainElement("ton"))
	})
	It("warns when TON wallets lack an API key but still registers", func() {
		var logs bytes.Buffer
		cfg := sampleCfg()
		cfg.Crypto.TONWallets = []string{"UQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
		Expect(clientIDs(clients.NewCryptoClients(cfg, zerolog.New(&logs), nil, nil, nil, nil, nil))).To(ContainElement("ton"))
		Expect(logs.String()).To(ContainSubstring("1 RPS anonymous quota"))
		cfg.Crypto.TONCenterAPIKey = "key"
		logs.Reset()
		Expect(clientIDs(clients.NewCryptoClients(cfg, zerolog.New(&logs), nil, nil, nil, nil, nil))).To(ContainElement("ton"))
		Expect(logs.String()).NotTo(ContainSubstring("1 RPS anonymous quota"))
	})
	It("registers Steam only when STEAM_ID is configured", func() {
		cfg := sampleCfg()
		Expect(clientIDs(clients.NewCryptoClients(cfg, zerolog.Nop(), nil, nil, nil, nil, nil))).NotTo(ContainElement("steam"))
		cfg.Steam.SteamID = "76561198000000000"
		Expect(clientIDs(clients.NewCryptoClients(cfg, zerolog.Nop(), nil, nil, nil, nil, nil))).To(ContainElement("steam"))
	})
	It("warns when Steam lacks a session but still registers", func() {
		var logs bytes.Buffer
		cfg := sampleCfg()
		cfg.Steam.SteamID = "76561198000000000"
		Expect(clientIDs(clients.NewCryptoClients(cfg, zerolog.New(&logs), nil, nil, nil, nil, nil))).To(ContainElement("steam"))
		Expect(logs.String()).To(ContainSubstring("only public inventory"))
		cfg.Steam.Session = "sessionid=x"
		logs.Reset()
		Expect(clientIDs(clients.NewCryptoClients(cfg, zerolog.New(&logs), nil, nil, nil, nil, nil))).To(ContainElement("steam"))
		Expect(logs.String()).NotTo(ContainSubstring("only public inventory"))
	})
	It("flattens configured clients into the existing fx group", func() {
		cfg := sampleCfg()
		repo := repomocks.NewMockActivityRepository(gomock.NewController(GinkgoT()))
		cursors := repomocks.NewMockCursorRepository(gomock.NewController(GinkgoT()))
		priceHistory := repomocks.NewMockPriceHistoryRepository(gomock.NewController(GinkgoT()))
		steamStore := repomocks.NewMockSteamAssetRepository(gomock.NewController(GinkgoT()))
		type inputs struct {
			fx.In
			Clients []domainsync.BrokerClient `group:"broker_clients"`
		}
		var ids []string
		app := fx.New(fx.NopLogger, fx.Supply(cfg, zerolog.Nop()),
			fx.Provide(func() repository.ActivityRepository { return repo }),
			fx.Provide(func() repository.CursorRepository { return cursors }),
			fx.Provide(func() repository.PriceHistoryRepository { return priceHistory }),
			fx.Provide(func() repository.SteamAssetRepository { return steamStore }),
			fx.Provide(func() *appprices.Service { return appprices.NewService(priceHistory, nil) }),
			clients.Module, fx.Invoke(func(in inputs) {
				for _, c := range in.Clients {
					ids = append(ids, c.ID())
				}
			}))
		Expect(app.Err()).NotTo(HaveOccurred())
		Expect(ids).To(ConsistOf("futu", "ibkr", "binance", "okx", "bitget", "hyperliquid", "okx_web3"))
	})
})
