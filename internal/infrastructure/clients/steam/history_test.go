package steam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Inventory history", func() {
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
		c.cfg.CommunityBase = server.URL
		return c
	}

	row := func(date, rest, attrs string) string {
		return `<div class="tradehistoryrow" data-appid="730" data-contextid="2" ` + attrs + `>` +
			`<div class="event_description">` + date + ` ` + rest + `</div></div>`
	}
	descs := map[string]any{
		"730": map[string]any{
			"1_0": map[string]any{"market_hash_name": "AK-47 | Redline (Field-Tested)"},
		},
	}

	page := func(html string, cursor any) map[string]any {
		m := map[string]any{
			"success": true, "html": html, "num": 2,
			"descriptions": descs,
			"apps":         []any{map[string]any{"appid": 730}},
		}
		if cursor != nil {
			m["cursor"] = cursor
		}
		return m
	}

	It("parses rows with kinds and follows the cursor exactly", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor[time]") == "" {
				Expect(r.URL.Query().Get("app[]")).To(Equal("730"))
				writeJSON(w, page(
					row("3 Aug, 2026 1:36am", "Traded - ★ Butterfly Knife | Ultraviolet", `data-classid="9" data-instanceid="0" data-amount="1"`)+
						row("7 Jul, 2026 2:55pm", "Earned + Premier Season Four Medal", `data-classid="2" data-instanceid="0" data-amount="1"`),
					map[string]any{"time": "1729999999", "time_frac": "0", "s": "abc"}))
				return
			}
			Expect(r.URL.Query().Get("cursor[time]")).To(Equal("1729999999"))
			Expect(r.URL.Query().Get("cursor[time_frac]")).To(Equal("0"))
			Expect(r.URL.Query().Get("cursor[s]")).To(Equal("abc"))
			writeJSON(w, page("", nil))
		}
		evs, gotDescs, complete, _, err := newClient().fetchHistory(context.Background(), nil, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(evs).To(HaveLen(2))
		Expect(evs[0].Kind).To(Equal("Traded"))
		Expect(evs[1].Kind).To(Equal("Earned"))
		Expect(evs[0].Quantity).To(Equal(1))
		Expect(gotDescs["1_0"].MarketHashName).To(Equal("AK-47 | Redline (Field-Tested)"))
	})

	It("starts newest without start_time anchors", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			Expect(r.URL.Query().Get("start_time")).To(BeEmpty())
			writeJSON(w, page("", nil))
		}
		_, _, complete, resume, err := newClient().fetchHistory(context.Background(), nil, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(resume).To(BeNil())
	})

	It("rejects unsuccessful envelopes as incomplete", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"success": false, "html": "", "descriptions": map[string]any{}})
		}
		_, _, complete, _, err := newClient().fetchHistory(context.Background(), nil, time.Time{})
		Expect(err).To(HaveOccurred())
		Expect(complete).To(BeFalse())
	})

	It("stops at the watermark on fresh walks", func() {
		// Watermark sits between the two rows: the walk imports the
		// page and stops complete without requesting older pages.
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			writeJSON(w, page(
				row("8 May, 2025 1:00pm", "Received + New", `data-classid="1" data-instanceid="0" data-amount="1"`)+
					row("1 May, 2025 1:00pm", "Received + Old", `data-classid="1" data-instanceid="0" data-amount="1"`),
				map[string]any{"time": "1", "time_frac": "0", "s": "x"}))
		}
		watermark := time.Date(2025, 5, 4, 0, 0, 0, 0, time.UTC)
		evs, _, complete, resume, err := newClient().fetchHistory(context.Background(), nil, watermark)
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(resume).To(BeNil())
		Expect(evs).To(HaveLen(2))
		Expect(calls).To(Equal(1))
	})

	It("resumes capped walks from the provider cursor", func() {
		calls := 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			writeJSON(w, page(
				row("8 May, 2025 1:00pm", "Received + AK", `data-classid="1" data-instanceid="0" data-amount="1"`),
				map[string]any{"time": "1", "time_frac": "0", "s": "x"}))
		}
		c := newClient()
		c.cfg.MaxPages = 1
		_, _, complete, resume, err := c.fetchHistory(context.Background(), nil, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(resume).NotTo(BeNil())
		// The resumed walk picks up where the cap stopped, ignoring the
		// watermark that the fresh walk would have stopped at.
		calls = 0
		handler = func(w http.ResponseWriter, r *http.Request) {
			calls++
			Expect(r.URL.Query().Get("cursor[time]")).To(Equal("1"))
			writeJSON(w, page(
				row("8 May, 2025 1:00pm", "Received + AK", `data-classid="1" data-instanceid="0" data-amount="1"`),
				map[string]any{"time": "1", "time_frac": "0", "s": "x"}))
		}
		_, _, complete, resume2, err := c.fetchHistory(context.Background(), resume, time.Now().UTC())
		Expect(err).NotTo(HaveOccurred())
		Expect(calls).To(Equal(1))
		Expect(complete).To(BeFalse())
		Expect(resume2).NotTo(BeNil())
	})

	It("dedupes repeated rows", func() {
		one := row("8 May, 2025 1:00pm", "Received + AK-47", `data-classid="1" data-instanceid="0" data-amount="1"`)
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, page(one+one, nil))
		}
		evs, _, complete, _, err := newClient().fetchHistory(context.Background(), nil, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeTrue())
		Expect(evs).To(HaveLen(1))
	})

	It("marks incomplete on cursor stall", func() {
		handler = func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, page(
				row("8 May, 2025 1:00pm", "Received + AK", `data-classid="1" data-instanceid="0"`),
				map[string]any{"time": "1", "time_frac": "0", "s": "x"}))
		}
		// Same cursor twice would loop forever; the second identical
		// cursor stops the walk as incomplete.
		evs, _, complete, _, err := newClient().fetchHistory(context.Background(), nil, time.Time{})
		Expect(err).NotTo(HaveOccurred())
		Expect(complete).To(BeFalse())
		Expect(evs).NotTo(BeEmpty())
	})

	It("normalizes verbs and keeps unknown ones visible", func() {
		Expect(normalizeHistoryKind("Purchased on Community Market foo")).To(Equal(EventMarketBuy))
		Expect(normalizeHistoryKind("Traded - something")).To(Equal(EventTraded))
		Expect(normalizeHistoryKind("Earned a new rank and got a drop")).To(Equal(EventEarned))
		Expect(normalizeHistoryKind("Unboxed greatness")).To(Equal(EventUnboxed))
		Expect(normalizeHistoryKind("Frobnicated")).To(Equal("other:Frobnicated"))
		Expect(normalizeHistoryKind("")).To(Equal("other"))
	})

	It("parses timestamps tolerantly", func() {
		Expect(parseSteamTime("1730000000").Unix()).To(Equal(int64(1730000000)))
		Expect(parseSteamTime("2025-05-08 13:22:00").Year()).To(Equal(2025))
		Expect(parseSteamTime("3 Aug, 2026 1:36am").Year()).To(Equal(2026))
		// Real /market/pricehistory/ shape: "Sep 13 2025 01: +0".
		d := parseSteamTime("Sep 13 2025 01: +0")
		Expect(d.IsZero()).To(BeFalse())
		Expect(d.Year()).To(Equal(2025))
		Expect(d.Month()).To(Equal(time.September))
		Expect(d.Day()).To(Equal(13))
		Expect(parseSteamTime("bogus").IsZero()).To(BeTrue())
		Expect(parseSteamTime("").IsZero()).To(BeTrue())
	})
})
