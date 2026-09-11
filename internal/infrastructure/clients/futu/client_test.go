package futu_test

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/santsai/futu-go/pb"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/futu"
)

func TestFutuClient(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Futu Client Suite")
}

// fakeSession lets us drive the client without a real OpenD daemon.
type fakeSession struct {
	unlocked  bool
	accs      []futu.Account
	fundsByID map[uint64]*pb.Funds
	posByID   map[uint64][]*pb.Position
	dealsByID map[uint64][]*pb.OrderFill
	fundsErr  map[uint64]error
	posErr    map[uint64]error
	dealsErr  map[uint64]error
	failOn    string // "unlock" | "list" | ""
}

func (f *fakeSession) Unlock(_ context.Context, _ string) error {
	if f.failOn == "unlock" {
		return errors.New("bad pwd")
	}
	f.unlocked = true
	return nil
}
func (f *fakeSession) ListAccounts(_ context.Context) ([]futu.Account, error) {
	if f.failOn == "list" {
		return nil, errors.New("denied")
	}
	return f.accs, nil
}
func (f *fakeSession) Funds(_ context.Context, a futu.Account) (*pb.Funds, error) {
	if err, ok := f.fundsErr[a.AccID]; ok {
		return nil, err
	}
	return f.fundsByID[a.AccID], nil
}
func (f *fakeSession) Positions(_ context.Context, a futu.Account) ([]*pb.Position, error) {
	if err, ok := f.posErr[a.AccID]; ok {
		return nil, err
	}
	return f.posByID[a.AccID], nil
}
func (f *fakeSession) HistoryDeals(_ context.Context, a futu.Account, _, _ time.Time) ([]*pb.OrderFill, error) {
	if err, ok := f.dealsErr[a.AccID]; ok {
		return nil, err
	}
	return f.dealsByID[a.AccID], nil
}
func (f *fakeSession) Close(_ context.Context) error { return nil }

// marketSplitSession returns different positions per market leg to simulate
// market-filtered upstream endpoints.
type marketSplitSession struct {
	*fakeSession
	hkPos []*pb.Position
	usPos []*pb.Position
	// usDealsErr fails history only on the US leg to simulate a partial fetch.
	usDealsErr error
}

func (m *marketSplitSession) Positions(_ context.Context, a futu.Account) ([]*pb.Position, error) {
	if a.TrdMarket == pb.TrdMarket_TrdMarket_US {
		return m.usPos, nil
	}
	return m.hkPos, nil
}

func (m *marketSplitSession) HistoryDeals(_ context.Context, a futu.Account, _, _ time.Time) ([]*pb.OrderFill, error) {
	if a.TrdMarket == pb.TrdMarket_TrdMarket_US && m.usDealsErr != nil {
		return nil, m.usDealsErr
	}
	return m.fakeSession.HistoryDeals(context.Background(), a, time.Time{}, time.Time{})
}

type fakeDialer struct {
	sess futu.Session
	err  error
}

func (f fakeDialer) Dial(_ context.Context, _ string, _ int) (futu.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sess, nil
}

func mkFunds(total, cash float64, cur pb.Currency) *pb.Funds {
	c := cur
	t := total
	cs := cash
	return &pb.Funds{Currency: &c, TotalAssets: &t, Cash: &cs}
}

func mkPosition(code, name string, qty, price, cost float64, cur pb.Currency) *pb.Position {
	c := cur
	avg := cost + 10 // average cost differs from (deprecated) cost price
	return &pb.Position{
		Code: &code, Name: &name, Qty: &qty, Price: &price,
		CostPrice: &cost, AverageCostPrice: &avg, Currency: &c,
	}
}

var _ = Describe("Client.ID", func() {
	It("returns futu", func() {
		Expect(futu.New("h", 11111, "p", "id", nil, fakeDialer{sess: &fakeSession{}}).ID()).To(Equal("futu"))
	})
})

var _ = Describe("Client.Fetch", func() {
	It("fails fast when host/port is empty", func() {
		c := futu.New("", 0, "", "", nil, fakeDialer{sess: &fakeSession{}})
		_, err := c.Fetch(context.Background())
		Expect(err).To(HaveOccurred())
	})

	It("propagates dial failures", func() {
		c := futu.New("h", 1, "pwd", "id", nil, fakeDialer{err: errors.New("nope")})
		_, err := c.Fetch(context.Background())
		Expect(err).To(MatchError(ContainSubstring("dial")))
	})

	It("propagates unlock failures", func() {
		c := futu.New("h", 1, "pwd", "id", nil, fakeDialer{sess: &fakeSession{failOn: "unlock"}})
		_, err := c.Fetch(context.Background())
		Expect(err).To(MatchError(ContainSubstring("unlock")))
	})

	It("translates one account with funds + positions", func() {
		acc := futu.Account{
			AccID:     281000123,
			TrdEnv:    pb.TrdEnv_TrdEnv_Real,
			TrdMarket: pb.TrdMarket_TrdMarket_HK,
			AccType:   pb.TrdAccType_TrdAccType_Margin,
			CardNum:   "281000123",
		}
		sess := &fakeSession{
			accs: []futu.Account{acc},
			fundsByID: map[uint64]*pb.Funds{
				acc.AccID: mkFunds(123456, 5000, pb.Currency_Currency_HKD),
			},
			posByID: map[uint64][]*pb.Position{
				acc.AccID: {mkPosition("00700", "Tencent", 100, 320, 280, pb.Currency_Currency_HKD)},
			},
		}
		c := futu.New("h", 1, "pwd", "id", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(sess.unlocked).To(BeTrue())
		Expect(snap.Connection.BrokerageSlug).To(Equal("futu"))
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Accounts[0].ID).To(Equal("futu-281000123"))
		Expect(snap.Accounts[0].BalanceTotal).To(Equal(123456.0))
		Expect(snap.Holdings).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions[0].Symbol.Symbol).To(Equal("0700.HK"))
		Expect(snap.Holdings[0].Balances[0].Currency.Code).To(Equal("HKD"))
		Expect(snap.Holdings[0].Balances[0].Cash).To(Equal(5000.0))
		// Basis uses AverageCostPrice, not the deprecated CostPrice.
		Expect(snap.Holdings[0].Positions[0].AveragePurchasePrice).To(Equal(290.0))
	})

	It("skips paper (simulated) accounts", func() {
		acc := futu.Account{
			AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Simulate,
			TrdMarket: pb.TrdMarket_TrdMarket_US, AccType: pb.TrdAccType_TrdAccType_Cash,
		}
		sess := &fakeSession{accs: []futu.Account{acc}}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(BeEmpty())
	})

	It("does not call unlock when password is blank", func() {
		acc := futu.Account{AccID: 1, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		sess := &fakeSession{accs: []futu.Account{acc}}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		_, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(sess.unlocked).To(BeFalse())
	})

	It("propagates ListAccounts failures", func() {
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: &fakeSession{failOn: "list"}})
		_, err := c.Fetch(context.Background())
		Expect(err).To(MatchError(ContainSubstring("list accounts")))
	})

	It("skips accounts where both Funds and Positions calls fail", func() {
		bad := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		good := futu.Account{AccID: 2, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		sess := &fakeSession{
			accs:     []futu.Account{bad, good},
			fundsErr: map[uint64]error{1: errors.New("f")},
			posErr:   map[uint64]error{1: errors.New("p")},
			fundsByID: map[uint64]*pb.Funds{
				2: mkFunds(10, 5, pb.Currency_Currency_HKD),
			},
		}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Accounts[0].ID).To(Equal("futu-2"))
	})

	It("keeps an account when only one of Funds/Positions fails", func() {
		acc := futu.Account{AccID: 7, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		sess := &fakeSession{
			accs:   []futu.Account{acc},
			posErr: map[uint64]error{7: errors.New("p only")},
			fundsByID: map[uint64]*pb.Funds{
				7: mkFunds(10, 5, pb.Currency_Currency_HKD),
			},
		}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions).To(BeEmpty())
	})
})

var _ = Describe("Translate (mapping logic)", func() {
	It("returns an empty snapshot when given no accounts", func() {
		snap := futu.Translate(nil)
		Expect(snap.Connection.BrokerageSlug).To(Equal("futu"))
		Expect(snap.Accounts).To(BeEmpty())
		Expect(snap.Holdings).To(BeEmpty())
	})

	It("maps OrderFill records into Activities and flips InitialTxSyncDone", func() {
		code := "00700"
		name := "Tencent"
		price := 320.0
		qty := 100.0
		fillIDEx := "fill-1"
		side := pb.TrdSide_TrdSide_Buy
		createdTS := float64(time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC).Unix())
		fill := &pb.OrderFill{
			Code:            &code,
			Name:            &name,
			Price:           &price,
			Qty:             &qty,
			FillIDEx:        &fillIDEx,
			TrdSide:         &side,
			CreateTimestamp: &createdTS,
		}
		acc := futu.Account{AccID: 9, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		snap := futu.Translate([]futu.AccountSnapshot{{
			Account:           acc,
			Deals:             []*pb.OrderFill{fill},
			ActivitiesFetched: true,
		}})
		Expect(snap.Activities).To(HaveKey("futu-9"))
		acts := snap.Activities["futu-9"]
		Expect(acts).To(HaveLen(1))
		Expect(acts[0].Type).To(BeEquivalentTo("BUY"))
		Expect(acts[0].SourceRecordID).To(Equal("fill-1"))
		Expect(acts[0].Symbol.Symbol).To(Equal("0700.HK"))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
	})

	It("keeps partial fills while leaving history incomplete", func() {
		code := "00700"
		name := "Tencent"
		price := 320.0
		qty := 100.0
		fillIDEx := "fill-1"
		side := pb.TrdSide_TrdSide_Buy
		createdTS := float64(time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC).Unix())
		fill := &pb.OrderFill{
			Code: &code, Name: &name, Price: &price, Qty: &qty,
			FillIDEx: &fillIDEx, TrdSide: &side, CreateTimestamp: &createdTS,
		}
		hkAcc := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		usAcc := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_US}
		snap := futu.Translate([]futu.AccountSnapshot{
			{
				Account: hkAcc,
				Funds:   mkFunds(100000, 5000, pb.Currency_Currency_HKD),
				Deals:   []*pb.OrderFill{fill},
				// US leg failed: merged history is incomplete even though
				// the HK leg produced a fill.
				ActivitiesFetched: false,
			},
			{
				Account:           usAcc,
				Deals:             []*pb.OrderFill{fill},
				ActivitiesFetched: true,
			},
		})
		// NOTE: Translate does not merge; Fetch does. Both legs translate
		// independently here to pin the per-leg completion rule.
		Expect(snap.Accounts).To(HaveLen(2))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(snap.Accounts[0].LastTxSync).To(BeNil())
		Expect(snap.Activities).NotTo(BeEmpty())
	})

	It("marks merged history incomplete when one market leg fails", func() {
		hkAcc := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		usAcc := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_US}
		code := "00700"
		name := "Tencent"
		price := 320.0
		qty := 100.0
		fillIDEx := "fill-1"
		side := pb.TrdSide_TrdSide_Buy
		createdTS := float64(time.Date(2026, 5, 1, 9, 30, 0, 0, time.UTC).Unix())
		inner := &fakeSession{
			accs: []futu.Account{hkAcc, usAcc},
			fundsByID: map[uint64]*pb.Funds{
				1: mkFunds(100000, 5000, pb.Currency_Currency_HKD),
			},
			dealsByID: map[uint64][]*pb.OrderFill{
				1: {{
					Code: &code, Name: &name, Price: &price, Qty: &qty,
					FillIDEx: &fillIDEx, TrdSide: &side, CreateTimestamp: &createdTS,
				}},
			},
		}
		sess := &marketSplitSession{fakeSession: inner, usDealsErr: errors.New("us history down")}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		// The HK fill imports, but history stays incomplete so the next
		// run retries the failed US scope instead of treating it done.
		Expect(snap.Activities["futu-1"]).To(HaveLen(1))
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
		Expect(snap.Accounts[0].LastTxSync).To(BeNil())
	})

	It("marks an empty successful HistoryDeals response as synced", func() {
		acc := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		sess := &fakeSession{
			accs: []futu.Account{acc},
			fundsByID: map[uint64]*pb.Funds{
				1: mkFunds(10000, 10000, pb.Currency_Currency_HKD),
			},
		}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Activities).To(BeEmpty())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeTrue())
		Expect(snap.Accounts[0].LastTxSync).NotTo(BeNil())
	})

	It("tolerates HistoryDeals failures by yielding zero activities", func() {
		acc := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		sess := &fakeSession{
			accs: []futu.Account{acc},
			fundsByID: map[uint64]*pb.Funds{
				1: mkFunds(10000, 10000, pb.Currency_Currency_HKD),
			},
			dealsErr: map[uint64]error{1: errors.New("boom")},
		}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Activities).To(BeEmpty())
		Expect(snap.Accounts[0].InitialTxSyncDone).To(BeFalse())
	})

	It("merges per-market snapshots of one universal account without doubling", func() {
		hk := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		us := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_US}
		side := pb.TrdSide_TrdSide_Buy
		fillIDEx := "fill-1"
		code := "00700"
		name := "Tencent"
		price := 320.0
		qty := 100.0
		created := float64(1700000000)
		fill := &pb.OrderFill{
			Code: &code, Name: &name, Price: &price, Qty: &qty,
			FillIDEx: &fillIDEx, TrdSide: &side, CreateTimestamp: &created,
		}
		sess := &fakeSession{
			// Account-wide upstream responses: both market legs return
			// the same funds, positions and fills.
			accs: []futu.Account{hk, us},
			fundsByID: map[uint64]*pb.Funds{
				1: mkFunds(100000, 5000, pb.Currency_Currency_HKD),
			},
			posByID: map[uint64][]*pb.Position{
				1: {mkPosition("00700", "Tencent", 100, 320, 280, pb.Currency_Currency_HKD)},
			},
			dealsByID: map[uint64][]*pb.OrderFill{
				1: {fill},
			},
		}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Accounts[0].ID).To(Equal("futu-1"))
		Expect(snap.Accounts[0].BalanceTotal).To(Equal(100000.0))
		Expect(snap.Holdings).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions).To(HaveLen(1))
		Expect(snap.Activities["futu-1"]).To(HaveLen(1))
	})

	It("unions distinct market positions and marks partial legs", func() {
		hkAcc := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		usAcc := futu.Account{AccID: 1, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_US}
		inner := &fakeSession{
			accs: []futu.Account{hkAcc, usAcc},
			fundsByID: map[uint64]*pb.Funds{
				1: mkFunds(100000, 5000, pb.Currency_Currency_HKD),
			},
		}
		sess := &marketSplitSession{
			fakeSession: inner,
			hkPos:       []*pb.Position{mkPosition("00700", "Tencent", 100, 320, 280, pb.Currency_Currency_HKD)},
			usPos:       []*pb.Position{mkPosition("AAPL", "Apple", 10, 200, 180, pb.Currency_Currency_USD)},
		}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Holdings).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions).To(HaveLen(2))
	})

	It("publishes emptied accounts instead of retaining stale holdings", func() {
		acc := futu.Account{AccID: 5, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		sess := &fakeSession{
			accs: []futu.Account{acc},
			fundsByID: map[uint64]*pb.Funds{
				5: mkFunds(0, 0, pb.Currency_Currency_HKD),
			},
			posByID: map[uint64][]*pb.Position{
				5: {},
			},
		}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Holdings).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions).To(BeEmpty())
		Expect(snap.Holdings[0].Partial).To(BeFalse())
	})

	It("marks holdings partial when positions fail but funds succeed", func() {
		acc := futu.Account{AccID: 7, TrdEnv: pb.TrdEnv_TrdEnv_Real, TrdMarket: pb.TrdMarket_TrdMarket_HK}
		sess := &fakeSession{
			accs:   []futu.Account{acc},
			posErr: map[uint64]error{7: errors.New("p only")},
			fundsByID: map[uint64]*pb.Funds{
				7: mkFunds(10, 5, pb.Currency_Currency_HKD),
			},
		}
		c := futu.New("h", 1, "", "", nil, fakeDialer{sess: sess})
		snap, err := c.Fetch(context.Background())
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.Accounts).To(HaveLen(1))
		Expect(snap.Holdings[0].Positions).To(BeEmpty())
		Expect(snap.Holdings[0].Partial).To(BeTrue())
	})
})
