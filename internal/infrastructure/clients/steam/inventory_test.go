package steam

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func testClient(srv *httptest.Server) *Client {
	cfg := defaultClientConfig()
	cfg.SteamID = "76561198000000000"
	cfg.MinInterval = time.Millisecond // New() treats <=0 as unset
	return New(cfg, srv.Client())
}

var _ = Describe("Inventory fetch", func() {
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

	page := func(assets, descs string, more bool, last string) map[string]any {
		var a, d []any
		Expect(json.Unmarshal([]byte(assets), &a)).To(Succeed())
		Expect(json.Unmarshal([]byte(descs), &d)).To(Succeed())
		m := map[string]any{"assets": a, "descriptions": d, "success": 1}
		if more {
			m["more_items"] = true
			m["last_assetid"] = last
		}
		return m
	}

	descs := `[{"classid":"1","instanceid":"0","market_hash_name":"AK-47 | Redline (Field-Tested)","marketable":1,"tradable":1,"name":"AK-47","type":"Rifle","tags":[{"category":"Rarity","internal_name":"rarity_rare"}]}]`

	It("joins assets to descriptions and stops without more_items", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Path).To(Equal("/inventory/76561198000000000/730/2"))
			writeJSON(w, page(
				`[{"assetid":"10","classid":"1","instanceid":"0","amount":"1"}]`,
				descs, false, ""))
		}
		items, complete, err := newClient().fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(items).To(HaveLen(1))
		Expect(items[0].MarketHashName).To(Equal("AK-47 | Redline (Field-Tested)"))
		Expect(items[0].Tags["Rarity"]).To(Equal("rarity_rare"))
		Expect(queries).To(HaveLen(1))
	})

	It("keeps assets without descriptions visible", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, page(
				`[{"assetid":"11","classid":"9","instanceid":"0","amount":2}]`,
				`[]`, false, ""))
		}
		items, complete, err := newClient().fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(items).To(HaveLen(1))
		Expect(items[0].MarketHashName).To(BeEmpty())
		Expect(items[0].Amount).To(Equal(2))
	})

	It("paginates with last_assetid while more_items", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("start_assetid") == "" {
				writeJSON(w, page(
					`[{"assetid":"10","classid":"1","instanceid":"0","amount":"1"}]`,
					descs, true, "10"))
				return
			}
			Expect(r.URL.Query().Get("start_assetid")).To(Equal("10"))
			writeJSON(w, page(
				`[{"assetid":"20","classid":"1","instanceid":"0","amount":"1"}]`,
				descs, false, ""))
		}
		items, complete, err := newClient().fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(items).To(HaveLen(2))
		Expect(items[1].AssetID).To(Equal("20"))
	})

	It("marks partial when the cursor stalls", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, page(
				`[{"assetid":"10","classid":"1","instanceid":"0","amount":"1"}]`,
				descs, true, ""))
		}
		items, complete, err := newClient().fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(items).To(HaveLen(1))
	})

	It("returns partial items with the page error", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				writeJSON(w, page(
					`[{"assetid":"10","classid":"1","instanceid":"0","amount":"1"}]`,
					descs, true, "10"))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, "boom")
		}
		c := newClient()
		c.cfg.MaxRetries = 0
		items, complete, err := c.fetchInventory(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(items).To(HaveLen(1))
	})

	It("fails fast on 403 without retrying", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusForbidden)
		}
		_, _, err := newClient().fetchInventory(context.Background())
		Expect(err).To(HaveOccurred())
		Expect(calls).To(Equal(1))
	})

	It("retries 429 then succeeds", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			writeJSON(w, page(`[]`, `[]`, false, ""))
		}
		items, complete, err := newClient().fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(items).To(BeEmpty())
		Expect(calls).To(Equal(2))
	})

	It("sends the session cookie when configured", func() {
		var gotCookie string
		handler = func(w http.ResponseWriter, r *http.Request) {
			gotCookie = r.Header.Get("Cookie")
			writeJSON(w, page(`[]`, `[]`, false, ""))
		}
		c := newClient()
		c.cfg.Session = "steamLoginSecure=sekret"
		_, _, err := c.fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(gotCookie).To(Equal("steamLoginSecure=sekret"))
	})

	It("normalizes odd amount shapes", func() {
		Expect(intVal(nil)).To(Equal(0))
		Expect(intVal("3")).To(Equal(3))
		Expect(intVal(4.0)).To(Equal(4))
		Expect(intVal("bogus")).To(Equal(0))
		Expect(truthy("1") && !truthy("0") && truthy(true) && !truthy(false)).To(BeTrue())
	})
})
