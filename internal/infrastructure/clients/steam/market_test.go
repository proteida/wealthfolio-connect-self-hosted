package steam

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Market history parsing", func() {
	row := func(inner string) string {
		return `<div class="market_listing_row" data-listingid="100" data-timestamp="1746720120">` + inner + `</div>`
	}

	It("parses a market buy", func() {
		tx, ok := parseMarketBlock(row(
			`<span class="market_listing_item_name">AK-47 | Redline (Field-Tested)</span>` +
				`<span>Purchased for -$23.41</span>` +
				`<span>8 May, 2025</span>`))
		Expect(ok).To(BeTrue())
		Expect(tx.Type).To(Equal("buy"))
		Expect(tx.MarketHashName).To(Equal("AK-47 | Redline (Field-Tested)"))
		Expect(tx.Gross).To(BeNumerically("~", 23.41, 1e-9))
		Expect(tx.Currency).To(Equal("USD"))
		Expect(tx.ExternalID).To(Equal("market:100"))
		Expect(tx.Net).To(BeNil()) // never derived by fee math
	})

	It("parses a sale and maps euro currency", func() {
		tx, ok := parseMarketBlock(row(
			`<span class="market_listing_item_name">AWP | Asiimov (Field-Tested)</span>` +
				`<span>Sold for +31,28€</span>` +
				`<span>May 9, 2025</span>`))
		Expect(ok).To(BeTrue())
		Expect(tx.Type).To(Equal("sell"))
		Expect(tx.Gross).To(BeNumerically("~", 31.28, 1e-9))
		Expect(tx.Currency).To(Equal("EUR"))
	})

	It("parses listings and cancellations", func() {
		tx, ok := parseMarketBlock(row(`<span>Listed on Community Market</span><span>9 May, 2025</span><span data-market-hash-name="AK"></span>`))
		Expect(ok).To(BeTrue())
		Expect(tx.Type).To(Equal("listing"))
		tx, ok = parseMarketBlock(row(`<span>Listing cancelled</span><span>9 May, 2025</span><span data-market-hash-name="AK"></span>`))
		Expect(ok).To(BeTrue())
		Expect(tx.Type).To(Equal("cancel"))
	})

	It("skips blocks without name or timestamp", func() {
		_, ok := parseMarketBlock(`<div>widget chrome with $1.00 but no item</div>`)
		Expect(ok).To(BeFalse())
		_, ok = parseMarketBlock(``)
		Expect(ok).To(BeFalse())
	})

	It("dedupes repeated rows across pages", func() {
		html := row(`<span class="market_listing_item_name">AK</span><span>- $5.00</span><span>8 May, 2025</span>`) +
			row(`<span class="market_listing_item_name">AK</span><span>- $5.00</span><span>8 May, 2025</span>`)
		Expect(parseMarketHTML(html)).To(HaveLen(1))
	})

	It("classifies by sign and words", func() {
		Expect(classifyMarketRow("- $5")).To(Equal("buy"))
		Expect(classifyMarketRow("+ $5")).To(Equal("sell"))
		Expect(classifyMarketRow("Purchased")).To(Equal("buy"))
		Expect(classifyMarketRow("???")).To(Equal("other"))
	})
})
