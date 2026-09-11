package steam

import (
	"context"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Trade history", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	BeforeEach(func() {
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	newClient := func() *Client {
		c := testClient(server)
		c.cfg.StoreBase = server.URL
		return c
	}

	trade := func(id string) map[string]any {
		return map[string]any{
			"tradeid": id, "steamid_other": "76561198000000001",
			"time_init": "1730000000", "status": "complete",
			"assets_given": []any{
				map[string]any{"appid": 730, "contextid": "2", "assetid": "10", "classid": "1", "instanceid": "0", "amount": "1"},
			},
			"assets_received": []any{
				map[string]any{"appid": 730, "contextid": "2", "assetid": "99", "classid": "2", "instanceid": "0", "amount": "1", "new_assetid": "4242", "new_contextid": "2"},
			},
		}
	}

	It("normalizes trades preserving new_assetid", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Query().Get("max_trades")).To(Equal("100"))
			Expect(r.URL.Query().Get("include_failed")).To(Equal("1"))
			writeJSON(w, map[string]any{"response": map[string]any{
				"more": false, "total_trades": 1, "trades": []any{trade("t1")},
			}})
		}
		recs, complete, err := newClient().fetchTrades(context.Background(), parseSteamTime("1700000000"))
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(recs).To(HaveLen(1))
		Expect(recs[0].TradeID).To(Equal("t1"))
		Expect(recs[0].Given[0].AssetID).To(Equal("10"))
		Expect(recs[0].Received[0].NewAssetID).To(Equal("4242"))
		Expect(recs[0].Received[0].Quantity).To(Equal(1))
	})

	It("paginates with cursors and skips id-less rows", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				writeJSON(w, map[string]any{"response": map[string]any{
					"more": true, "trades": []any{trade("t1"), map[string]any{"no": "id"}},
				}})
				return
			}
			Expect(r.URL.Query().Get("start_after_tradeid")).To(Equal("t1"))
			writeJSON(w, map[string]any{"response": map[string]any{"more": false, "trades": []any{}}})
		}
		recs, complete, err := newClient().fetchTrades(context.Background(), parseSteamTime("0"))
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(recs).To(HaveLen(1))
		Expect(calls).To(Equal(2))
	})
})
