package steam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"time"

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
			"tradeid": id, "steamid_other": "76561199495663064",
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
		recs, complete, _, err := newClient().fetchTrades(context.Background(), nil, time.Time{})
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
		recs, complete, _, err := newClient().fetchTrades(context.Background(), nil, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(recs).To(HaveLen(1))
		Expect(calls).To(Equal(2))
	})

	It("starts newest without seeding newer timestamps", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Query().Get("start_after_time")).To(BeEmpty())
			writeJSON(w, map[string]any{"response": map[string]any{"more": false, "trades": []any{}}})
		}
		_, complete, resume, err := newClient().fetchTrades(context.Background(), nil, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(resume).To(BeNil())
	})

	It("stops at the watermark and resumes capped walks", func() {
		newTrade := func(id, ts string) map[string]any {
			t := trade(id)
			t["time_init"] = ts
			return t
		}
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"response": map[string]any{
				"more": true, "trades": []any{newTrade("t2", "1746720120"), newTrade("t1", "1700000000")},
			}})
		}
		c := newClient()
		// Watermark between the rows: the fresh walk imports the page
		// and stops complete instead of paging forever on more=true.
		watermark := parseSteamTime("1720000000")
		recs, complete, resume, err := c.fetchTrades(context.Background(), nil, watermark)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(resume).To(BeNil())
		Expect(recs).To(HaveLen(2))
	})

	It("returns a resume offset when the page cap hits", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"response": map[string]any{
				"more": true, "trades": []any{trade("t1")},
			}})
		}
		c := newClient()
		c.cfg.MaxPages = 1
		_, complete, resume, err := c.fetchTrades(context.Background(), nil, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(resume).NotTo(BeNil())
		Expect(resume.AfterID).To(Equal("t1"))
		// The resumed walk seeds the provider offset and ignores the
		// watermark it already sits below.
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Query().Get("start_after_tradeid")).To(Equal("t1"))
			writeJSON(w, map[string]any{"response": map[string]any{
				"more": true, "trades": []any{trade("t1")},
			}})
		}
		_, complete, resume2, err := c.fetchTrades(context.Background(), resume, time.Now().UTC())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(resume2).NotTo(BeNil())
	})
})
