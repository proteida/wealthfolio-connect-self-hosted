package binance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("broken body") }

var _ = Describe("Binance request weight guard", func() {
	var guard *weightTransport
	var now time.Time
	var calls int
	var status int
	var headers http.Header
	var body string
	request := func(path string) *http.Request {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.test"+path, nil)
		Expect(err).NotTo(HaveOccurred())
		return r
	}
	BeforeEach(func() {
		now = time.Unix(1800000000, 0)
		calls = 0
		status = 200
		headers = make(http.Header)
		body = `[]`
		guard = newWeightTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		guard.now = func() time.Time { return now }
	})
	send := func(path string) error {
		resp, err := guard.RoundTrip(request(path))
		if resp != nil {
			resp.Body.Close()
		}
		return err
	}
	It("counts balances, prices, metadata and trades against the same rolling 600 budget", func() {
		Expect(send("/api/v3/account")).To(Succeed())
		Expect(send("/api/v3/exchangeInfo")).To(Succeed())
		for range 5 {
			Expect(send("/api/v3/ticker/price")).To(Succeed())
		}
		for range 27 {
			Expect(send("/api/v3/myTrades")).To(Succeed())
		}
		Expect(calls).To(Equal(34))
		Expect(send("/api/v3/myTrades")).To(MatchError(ContainSubstring("600")))
		Expect(calls).To(Equal(34))
		now = now.Add(time.Minute - time.Nanosecond)
		Expect(send("/api/v3/account")).To(HaveOccurred())
		now = now.Add(time.Nanosecond)
		Expect(send("/api/v3/account")).To(Succeed())
		Expect(calls).To(Equal(35))
	})
	It("does not exceed the weight cap even under concurrent callers", func() {
		var successes atomic.Int64
		var wg sync.WaitGroup
		req := request("/api/v3/myTrades")
		for range 100 {
			wg.Go(func() {
				resp, err := guard.RoundTrip(req.Clone(context.Background()))
				if err == nil {
					successes.Add(1)
					resp.Body.Close()
				}
			})
		}
		wg.Wait()
		Expect(successes.Load()).To(Equal(int64(30)))
		Expect(calls).To(Equal(30))
	})
	It("stops before making another call when observed shared-IP weight is high", func() {
		headers.Set("X-MBX-USED-WEIGHT-1M", "5900")
		Expect(send("/api/v3/account")).To(Succeed())
		Expect(send("/api/v3/ticker/price")).To(HaveOccurred())
		Expect(calls).To(Equal(1))
		now = now.Add(time.Minute)
		headers.Del("X-MBX-USED-WEIGHT-1M")
		Expect(send("/api/v3/account")).To(Succeed())
	})
	DescribeTable("backs off without retrying and preserves SDK error bodies",
		func(code int, retry string, delay time.Duration) {
			status = code
			headers.Set("Retry-After", retry)
			body = `{"code":-1003,"msg":"Too much request weight used"}`
			resp, err := guard.RoundTrip(request("/api/v3/myTrades"))
			Expect(err).NotTo(HaveOccurred())
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			Expect(err).NotTo(HaveOccurred())
			Expect(string(raw)).To(Equal(body))
			now = now.Add(delay - time.Nanosecond)
			Expect(send("/api/v3/account")).To(HaveOccurred())
			Expect(calls).To(Equal(1))
			now = now.Add(time.Nanosecond)
			status = 200
			body = `[]`
			Expect(send("/api/v3/account")).To(Succeed())
			Expect(calls).To(Equal(2))
		},
		Entry("429 Retry-After seconds", 429, "120", 2*time.Minute),
		Entry("429 missing Retry-After", 429, "", time.Minute),
		Entry("418 ban", 418, "", 5*time.Minute),
		Entry("400 with -1003", 400, "", time.Minute),
		Entry("HTTP date", 429, time.Unix(1800000120, 0).UTC().Format(http.TimeFormat), 2*time.Minute),
	)
	It("does not treat unrelated API errors as a ban", func() {
		status = 400
		body = `{"code":-1121}`
		Expect(send("/api/v3/exchangeInfo")).To(Succeed())
		Expect(send("/api/v3/account")).To(Succeed())
		Expect(calls).To(Equal(2))
	})
	It("propagates canceled requests and transport/body errors", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := guard.RoundTrip(request("/api/v3/account").WithContext(ctx))
		Expect(err).To(MatchError(context.Canceled))
		Expect(calls).To(BeZero())
		guard.base = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("offline") })
		Expect(send("/api/v3/account")).To(MatchError("offline"))
		guard.base = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 429, Body: io.NopCloser(brokenBody{})}, nil
		})
		Expect(send("/api/v3/account")).To(MatchError(ContainSubstring("broken body")))
	})
	It("keeps the later cooldown", func() { Expect(maxTime(now.Add(time.Hour), now)).To(Equal(now.Add(time.Hour))) })
})
