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
	cfg.SteamID = "76561199495663064"
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
			Expect(r.URL.Path).To(Equal("/inventory/76561199495663064/730/2"))
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

	It("joins fungible instance spellings (Gamma 2 Case style)", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			// Asset carries instanceid "0" while its description carries
			// "": both mean "no instance" and must join.
			writeJSON(w, page(
				`[{"assetid":"30","classid":"7","instanceid":"0","amount":"3"}]`,
				`[{"classid":"7","instanceid":"","market_hash_name":"Gamma 2 Case","marketable":1,"tradable":1,"name":"Gamma 2 Case","type":"Container"}]`, false, ""))
		}
		items, complete, err := newClient().fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(items).To(HaveLen(1))
		Expect(items[0].MarketHashName).To(Equal("Gamma 2 Case"))
		Expect(items[0].Amount).To(Equal(3))
	})

	It("joins descriptions split across page boundaries", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("start_assetid") == "" {
				// First page holds the asset but not its description.
				writeJSON(w, page(
					`[{"assetid":"10","classid":"1","instanceid":"0","amount":"1"}]`,
					`[]`, true, "10"))
				return
			}
			// Second page holds the description for the earlier asset.
			writeJSON(w, page(
				`[{"assetid":"20","classid":"1","instanceid":"0","amount":"1"}]`,
				descs, false, ""))
		}
		items, complete, err := newClient().fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(items).To(HaveLen(2))
		for _, it := range items {
			Expect(it.MarketHashName).To(Equal("AK-47 | Redline (Field-Tested)"))
		}
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

var _ = Describe("Retry-After handling", func() {
	It("parses seconds and dates", func() {
		Expect(retryAfter(http.Header{"Retry-After": {"2"}})).To(Equal(2 * time.Second))
		Expect(retryAfter(http.Header{})).To(BeZero())
		Expect(retryAfter(http.Header{"Retry-After": {"bogus"}})).To(BeZero())
		future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
		Expect(retryAfter(http.Header{"Retry-After": {future}})).To(BeNumerically("~", 90*time.Second, 5*time.Second))
	})

	It("honors Retry-After on 429 then succeeds", func() {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer GinkgoRecover()
			calls++
			if calls == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			writeJSON(w, map[string]any{"success": 1, "assets": []any{}, "descriptions": []any{}})
		}))
		defer server.Close()
		c := testClient(server)
		c.cfg.CommunityBase = server.URL
		items, complete, err := c.fetchInventory(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(items).To(BeEmpty())
		Expect(calls).To(Equal(2))
	})
})
