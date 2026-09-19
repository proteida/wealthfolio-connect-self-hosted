package steam_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/rs/zerolog"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/steam"
)

func TestSteam(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Steam Suite")
}

var _ = Describe("Client wiring", func() {
	It("exposes slug, setters and commit acknowledgement", func() {
		c := steam.New(steam.ClientConfig{SteamID: "1"}, nil)
		Expect(c.ID()).To(Equal("steam"))
		c.SetLogger(zerolog.Nop())
		c.ConfigureHistory(nil)
		c.ConfigureCursors(nil)
		c.SetPriceService(nil)
		c.SetSteamStore(nil)
		c.SnapshotCommitted() // no-op without tentative progress
		Expect(c.ID()).To(Equal("steam"))
	})
})
