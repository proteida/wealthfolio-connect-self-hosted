package steam

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Inventory history", func() {
	var server *httptest.Server
	var handler http.HandlerFunc
	var queries []string
	BeforeEach(func() {
		queries = nil
		server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			queries = append(queries, r.URL.RequestURI())
			handler(w, r)
		}))
	})
	AfterEach(func() { server.Close() })

	newClient := func() *Client {
		c := testClient(server)
		c.cfg.CommunityBase = server.URL
		return c
	}

	It("follows the returned cursor exactly", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor[time]") == "" {
				writeJSON(w, map[string]any{
					"events": []any{
						map[string]any{"event_name": "Traded", "assetid": "10", "classid": "1", "instanceid": "0", "market_hash_name": "AK", "time": "1730000000"},
					},
					"cursor": map[string]any{"time": "1729999999", "time_frac": "0", "s": "abc"},
					"more":   true,
				})
				return
			}
			Expect(r.URL.Query().Get("cursor[time]")).To(Equal("1729999999"))
			Expect(r.URL.Query().Get("cursor[time_frac]")).To(Equal("0"))
			Expect(r.URL.Query().Get("cursor[s]")).To(Equal("abc"))
			writeJSON(w, map[string]any{"events": []any{}, "more": false})
		}
		evs, complete, err := newClient().fetchHistory(context.Background(), 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(evs).To(HaveLen(1))
		Expect(evs[0].Kind).To(Equal("Traded"))
		Expect(evs[0].Quantity).To(Equal(1))
	})

	It("sends start_time when importing older ranges", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Query().Get("start_time")).To(Equal("1700000000"))
			writeJSON(w, map[string]any{"events": []any{}})
		}
		_, complete, err := newClient().fetchHistory(context.Background(), 1700000000)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
	})

	It("dedupes repeated events and derives synthetic IDs", func() {
		row := map[string]any{"type": "Received", "classid": "1", "market_hash_name": "AK", "quantity": 2, "time": "1730000000"}
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"history": []any{row, row}, "has_more": false})
		}
		evs, complete, err := newClient().fetchHistory(context.Background(), 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(evs).To(HaveLen(1))
		Expect(evs[0].Quantity).To(Equal(2))
		Expect(evs[0].ExternalID).To(ContainSubstring("hist:"))
	})

	It("marks incomplete on cursor anomalies", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"events": []any{map[string]any{"type": "Earned", "time": "1730000000"}},
				"cursor": "bogus",
				"more":   true,
			})
		}
		evs, complete, err := newClient().fetchHistory(context.Background(), 0)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(evs).To(HaveLen(1))
	})

	It("keeps unknown kinds verbatim", func() {
		var generic map[string]any
		raw, _ := json.Marshal(map[string]any{"event_name": "Mysterious Future Event", "time": "1730000000"})
		Expect(json.Unmarshal(raw, &generic)).To(Succeed())
		ev := parseHistoryEvent(raw)
		Expect(ev.Kind).To(Equal("Mysterious Future Event"))
	})

	It("parses timestamps tolerantly", func() {
		Expect(parseSteamTime("1730000000").Unix()).To(Equal(int64(1730000000)))
		Expect(parseSteamTime("2025-05-08 13:22:00").Year()).To(Equal(2025))
		Expect(parseSteamTime("bogus").IsZero()).To(BeTrue())
		Expect(parseSteamTime("").IsZero()).To(BeTrue())
	})
})
