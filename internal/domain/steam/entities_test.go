package steam_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/steam"
)

func TestSteam(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Steam Suite")
}

var _ = Describe("PnL", func() {
	price := 31.28

	It("computes value, P&L and percent on a known basis", func() {
		basis := 23.41
		got := steam.PnL(steam.SteamAsset{Amount: 1, CostBasis: &basis}, &price, "steam_median")
		Expect(*got.CurrentValue).To(BeNumerically("~", 31.28, 1e-9))
		Expect(*got.UnrealizedPnL).To(BeNumerically("~", 7.87, 1e-9))
		Expect(*got.UnrealizedPnLPercent).To(BeNumerically("~", 33.62, 0.01))
	})

	It("keeps unknown basis distinct from break-even", func() {
		got := steam.PnL(steam.SteamAsset{Amount: 1}, &price, "steam_median")
		Expect(*got.CurrentValue).To(BeNumerically("~", 31.28, 1e-9))
		Expect(got.UnrealizedPnL).To(BeNil())
		Expect(got.UnrealizedPnLPercent).To(BeNil())
	})

	It("reports value without percent on a zero basis", func() {
		zero := 0.0
		got := steam.PnL(steam.SteamAsset{Amount: 2, CostBasis: &zero}, &price, "steam_median")
		Expect(*got.CurrentValue).To(BeNumerically("~", 62.56, 1e-9))
		Expect(*got.UnrealizedPnL).To(BeNumerically("~", 62.56, 1e-9))
		Expect(got.UnrealizedPnLPercent).To(BeNil())
	})

	It("yields nothing without a current price", func() {
		basis := 23.41
		got := steam.PnL(steam.SteamAsset{Amount: 1, CostBasis: &basis}, nil, "")
		Expect(got.CurrentValue).To(BeNil())
		Expect(got.UnrealizedPnL).To(BeNil())
	})
})
