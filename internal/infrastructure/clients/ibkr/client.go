// Package ibkr implements a BrokerClient that talks directly to a locally
// running Interactive Brokers Gateway (or TWS) via the github.com/scmhub/ibapi
// SDK.
//
// IB's API is asynchronous and event-driven: every request triggers a
// stream of callbacks ending in a sentinel like PositionEnd /
// AccountSummaryEnd. We translate that into a synchronous Fetch() by
// implementing a tiny EWrapper that buffers callbacks and signals
// completion through channels.
package ibkr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/scmhub/ibapi"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	domainsync "github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/sync"
)

// Connector abstracts the IB SDK so unit tests can plug in a fake.
type Connector interface {
	Fetch(ctx context.Context, host string, port int, clientID int64, accountFilter string) (RawSnapshot, error)
}

// RawSnapshot is the per-account raw payload returned by the gateway,
// before brokerage-domain translation. Public for testability.
type RawSnapshot struct {
	Accounts   map[string]*RawAccount // keyed by IB account id (e.g. "DU1234567")
	Positions  []RawPosition
	Executions []RawExecution
}

// RawAccount holds the subset of AccountSummary tags we care about, all in
// the account's native currency.
type RawAccount struct {
	AccountID      string
	NetLiquidation float64
	TotalCash      float64
	BuyingPower    float64
	UnrealizedPnL  float64
	RealizedPnL    float64
	Currency       string
}

// RawPosition is one position row. Derivative contracts carry their full
// identity: without ConID/strike/expiry/right, different option series
// collapse into one indistinguishable row.
type RawPosition struct {
	Account     string
	Symbol      string
	SecType     string // STK, OPT, FUT, CASH, ...
	Exchange    string
	Currency    string
	Quantity    float64
	AvgCost     float64
	ConID       int64
	LocalSymbol string // e.g. "AAPL  260116C00250000"
	Strike      float64
	Expiry      string // LastTradeDateOrContractMonth: "YYYYMMDD" for options
	Right       string // "C"/"CALL" or "P"/"PUT"
	Multiplier  float64
}

// RawExecution is one execution (fill) row from the IB Gateway.
type RawExecution struct {
	ExecID      string
	Account     string
	Symbol      string
	SecType     string
	Exchange    string
	Currency    string
	Side        string // "BOT" or "SLD"
	Shares      float64
	Price       float64
	Time        string // "YYYYMMDD HH:MM:SS" format from IB
	ConID       int64
	LocalSymbol string
	Strike      float64
	Expiry      string
	Right       string
	Multiplier  float64
}

// Client is the IBKR BrokerClient.
type Client struct {
	host          string
	port          int
	clientID      int64
	accountFilter string // empty means "all accounts"
	conn          Connector
}

// New builds a client. Pass nil connector to use a real IB Gateway connection.
func New(host string, port int, clientID int64, accountFilter string, c Connector) *Client {
	if c == nil {
		c = realConnector{}
	}
	return &Client{
		host:          host,
		port:          port,
		clientID:      clientID,
		accountFilter: accountFilter,
		conn:          c,
	}
}

// ID returns the slug used by sync orchestration.
func (c *Client) ID() string { return "ibkr" }

// Fetch opens a single IB Gateway session, requests every authorized
// account's summary + positions, and translates the result.
func (c *Client) Fetch(ctx context.Context) (domainsync.BrokerSnapshot, error) {
	if c.host == "" || c.port == 0 {
		return domainsync.BrokerSnapshot{}, errors.New("ibkr: gateway host/port not configured")
	}
	raw, err := c.conn.Fetch(ctx, c.host, c.port, c.clientID, c.accountFilter)
	if err != nil {
		return domainsync.BrokerSnapshot{}, err
	}
	return Translate(raw), nil
}

// Translate converts the raw gateway payload into a BrokerSnapshot. Public
// so unit tests can drive the mapping without touching the network.
func Translate(raw RawSnapshot) domainsync.BrokerSnapshot {
	now := time.Now().UTC()

	connection := brokerage.Connection{
		ID:              "ibkr-conn",
		AuthorizationID: "ibkr-auth",
		BrokerageName:   "Interactive Brokers",
		BrokerageSlug:   "ibkr",
		DisplayName:     "Interactive Brokers",
		Name:            "IBKR",
		Status:          brokerage.ConnectionActive,
		UpdatedAt:       now,
	}

	accounts := make([]brokerage.Account, 0, len(raw.Accounts))
	holdings := make([]brokerage.Holdings, 0, len(raw.Accounts))

	// Group positions by account.
	posByAcc := map[string][]RawPosition{}
	for _, p := range raw.Positions {
		posByAcc[p.Account] = append(posByAcc[p.Account], p)
	}

	for accID, summary := range raw.Accounts {
		cur := summary.Currency
		if cur == "" {
			cur = "USD"
		}
		acc := brokerage.Account{
			ID:                     "ibkr-" + accID,
			Name:                   "IBKR " + accID,
			AccountNumber:          accID,
			Type:                   brokerage.AccountTypeMargin,
			RawType:                "MARGIN",
			Currency:               cur,
			BalanceTotal:           summary.NetLiquidation,
			BalanceCurrency:        cur,
			BrokerageAuthorization: "ibkr-auth",
			InstitutionName:        "Interactive Brokers",
			SyncEnabled:            true,
			IsPaper:                strings.HasPrefix(accID, "DU"),
			Status:                 "open",
			CreatedDate:            now,
			LastTxSync:             &now,
			LastHoldingsSync:       &now,
			InitialTxSyncDone:      true,
			InitialHoldingsDone:    true,
		}
		accounts = append(accounts, acc)

		positions := make([]brokerage.Position, 0, len(posByAcc[accID]))
		options := make([]brokerage.OptionPosition, 0)
		for _, p := range posByAcc[accID] {
			if p.Quantity == 0 {
				continue
			}
			if p.SecType == "OPT" {
				options = append(options, optionPosition(p))
				continue
			}
			typeCode := ibSecTypeToCode(p.SecType)
			ex := p.Exchange
			if ex == "" {
				ex = "SMART"
			}
			sym := formatSymbol(p.Symbol, p.Exchange, p.Currency)
			positions = append(positions, brokerage.Position{
				Symbol: brokerage.Symbol{
					Symbol:    sym,
					RawSymbol: p.Symbol,
					Name:      sym,
					Type:      brokerage.SymbolType{Code: typeCode, IsSupported: true, Description: typeCode},
					Exchange:  brokerage.Exchange{Code: ex},
					Currency:  brokerage.Currency{Code: p.Currency},
				},
				Units:                p.Quantity,
				AveragePurchasePrice: p.AvgCost,
				Currency:             brokerage.Currency{Code: p.Currency},
			})
		}

		holdings = append(holdings, brokerage.Holdings{
			AccountID: acc.ID,
			Balances: []brokerage.Balance{{
				Currency:    brokerage.Currency{Code: cur},
				Cash:        summary.TotalCash,
				BuyingPower: summary.BuyingPower,
			}},
			Positions:       positions,
			OptionPositions: options,
			CapturedAt:      now,
		})
	}

	// Build activities from executions.
	activities := map[string][]brokerage.Activity{}
	for _, e := range raw.Executions {
		accID := "ibkr-" + e.Account
		actType := brokerage.ActivityBuy
		if e.Side == "SLD" {
			actType = brokerage.ActivitySell
		}
		ts, ok := parseIBTime(e.Time)
		if !ok {
			// Reject unparseable timestamps instead of stamping today:
			// a moving trade date rewrites history on every sync.
			continue
		}
		sym := formatSymbol(e.Symbol, e.Exchange, e.Currency)
		rawSym := e.Symbol
		mult := e.Multiplier
		if mult <= 0 {
			mult = 1
		}
		var optSym *brokerage.OptionSymbol
		if e.SecType == "OPT" {
			rawSym = e.LocalSymbol
			if rawSym == "" {
				rawSym = occSymbol(e.Symbol, e.Expiry, e.Right, e.Strike)
			}
			optSym = &brokerage.OptionSymbol{
				Ticker:         sym,
				OptionType:     optionSide(e.Right),
				StrikePrice:    e.Strike,
				ExpirationDate: parseOptionExpiry(e.Expiry),
				IsMiniOption:   mult != 100,
				Underlying: brokerage.Symbol{
					Symbol:    sym,
					RawSymbol: e.Symbol,
					Type:      brokerage.SymbolType{Code: "EQUITY", IsSupported: true},
					Exchange:  brokerage.Exchange{Code: e.Exchange},
					Currency:  brokerage.Currency{Code: e.Currency},
				},
			}
		}
		activities[accID] = append(activities[accID], brokerage.Activity{
			ID:        e.ExecID,
			AccountID: accID,
			Type:      actType,
			RawType:   e.Side,
			TradeDate: ts,
			Price:     e.Price,
			Units:     e.Shares,
			// Contract multiplier applies: one standard 100-multiplier
			// option at a $2 premium moves $200, not $2.
			Amount:       e.Price * e.Shares * mult,
			Currency:     brokerage.Currency{Code: e.Currency},
			OptionSymbol: optSym,
			Symbol: &brokerage.Symbol{
				Symbol:    sym,
				RawSymbol: rawSym,
				Name:      sym,
				Type:      brokerage.SymbolType{Code: ibSecTypeToCode(e.SecType), IsSupported: true},
				Exchange:  brokerage.Exchange{Code: e.Exchange},
				Currency:  brokerage.Currency{Code: e.Currency},
			},
			ProviderType:   "ibkr",
			SourceSystem:   "ibkr",
			SourceRecordID: e.ExecID,
		})
	}

	return domainsync.BrokerSnapshot{
		Connection: connection,
		Accounts:   accounts,
		Holdings:   holdings,
		Activities: activities,
	}
}

// ===================== real IB Gateway connector =====================

// realConnector connects to a real IB Gateway / TWS. Each Fetch opens a
// fresh session, waits for the data, then disconnects.
type realConnector struct{}

// Fetch opens a fresh IB Gateway session, gathers the account summary and
// open positions, then disconnects.
func (realConnector) Fetch(ctx context.Context, host string, port int, clientID int64, accountFilter string) (RawSnapshot, error) {
	w := newGatherWrapper(accountFilter)
	client := ibapi.NewEClient(w)
	if err := client.Connect(host, port, clientID); err != nil {
		return RawSnapshot{}, fmt.Errorf("ibkr: connect: %w", err)
	}
	defer func() { _ = client.Disconnect() }() //nolint:errcheck // best-effort cleanup; deferred Disconnect errors are not actionable

	// Wait for the Gateway to signal readiness via NextValidID before
	// issuing any data requests. Without this, requests sent immediately
	// after Connect may be silently dropped.
	if err := w.waitReady(ctx); err != nil {
		return RawSnapshot{}, err
	}

	// AccountSummary tags we care about. "All" group means every linked account.
	tags := strings.Join([]string{
		ibapi.NetLiquidation,
		ibapi.TotalCashValue,
		ibapi.BuyingPower,
	}, ",")

	const summaryReqID int64 = 9001
	client.ReqAccountSummary(summaryReqID, "All", tags)
	if err := w.waitAccountSummary(ctx); err != nil {
		return RawSnapshot{}, err
	}
	client.CancelAccountSummary(summaryReqID)

	client.ReqPositions()
	if err := w.waitPositions(ctx); err != nil {
		return RawSnapshot{}, err
	}
	client.CancelPositions()

	// Request execution details (fills) for all accounts. The empty
	// ExecutionFilter returns every execution from the current and
	// previous trading day (IB's server-side default).
	const execReqID int64 = 9002
	filter := ibapi.NewExecutionFilter()
	if accountFilter != "" {
		filter.AcctCode = accountFilter
	}
	client.ReqExecutions(execReqID, filter)
	if err := w.waitExecDetails(ctx); err != nil {
		return RawSnapshot{}, err
	}

	return w.snapshot(), nil
}

// gatherWrapper implements the (very large) ibapi.EWrapper interface by
// embedding the SDK's default no-op Wrapper and overriding only the
// callbacks we care about. It buffers data in-memory and exposes channels
// to wait for the *End sentinels.
type gatherWrapper struct {
	ibapi.Wrapper // default no-op implementations for every other callback

	mu sync.Mutex

	accountFilter string
	accounts      map[string]*RawAccount
	positions     []RawPosition
	executions    []RawExecution

	readyCh     chan struct{} // closed when NextValidID is received (API ready)
	readyOnce   sync.Once
	summaryDone chan struct{}
	summaryOnce sync.Once
	posDone     chan struct{}
	posOnce     sync.Once
	execDone    chan struct{}
	execOnce    sync.Once
	errCh       chan error
}

func newGatherWrapper(filter string) *gatherWrapper {
	return &gatherWrapper{
		accountFilter: filter,
		accounts:      map[string]*RawAccount{},
		readyCh:       make(chan struct{}),
		summaryDone:   make(chan struct{}),
		posDone:       make(chan struct{}),
		execDone:      make(chan struct{}),
		errCh:         make(chan error, 4),
	}
}

func (w *gatherWrapper) snapshot() RawSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := RawSnapshot{
		Accounts:   make(map[string]*RawAccount, len(w.accounts)),
		Positions:  append([]RawPosition(nil), w.positions...),
		Executions: append([]RawExecution(nil), w.executions...),
	}
	for k, v := range w.accounts {
		acc := *v
		out.Accounts[k] = &acc
	}
	return out
}

func (w *gatherWrapper) waitReady(ctx context.Context) error {
	return waitOrErr(ctx, w.readyCh, w.errCh, "ibkr: API ready timeout (NextValidID not received)")
}

func (w *gatherWrapper) waitAccountSummary(ctx context.Context) error {
	return waitOrErr(ctx, w.summaryDone, w.errCh, "ibkr: AccountSummary timeout")
}

func (w *gatherWrapper) waitPositions(ctx context.Context) error {
	return waitOrErr(ctx, w.posDone, w.errCh, "ibkr: Positions timeout")
}

func (w *gatherWrapper) waitExecDetails(ctx context.Context) error {
	return waitOrErr(ctx, w.execDone, w.errCh, "ibkr: ExecDetails timeout")
}

func waitOrErr(ctx context.Context, done <-chan struct{}, errCh <-chan error, label string) error {
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	select {
	case <-done:
		return nil
	case err := <-errCh:
		return err
	case <-timeout.C:
		return errors.New(label)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ----- Overridden EWrapper callbacks -----
//
// All overrides use POINTER receivers so that NewEClient(w *gatherWrapper)
// dispatches to our methods (ibapi.Wrapper itself uses value receivers, so
// by Go's method-set rules our *gatherWrapper methods take precedence).

func (w *gatherWrapper) acctOK(account string) bool {
	if w.accountFilter == "" {
		return true
	}
	return account == w.accountFilter
}

// NextValidID is called by the Gateway once the API session is fully ready.
// We use it as the signal that the connection is established and requests can
// be safely issued.
func (w *gatherWrapper) NextValidID(orderID int64) {
	w.readyOnce.Do(func() { close(w.readyCh) })
}

// AccountSummary records one summary tag for an account; called repeatedly by
// the IB Gateway client until AccountSummaryEnd.
func (w *gatherWrapper) AccountSummary(reqID int64, account, tag, value, currency string) {
	if !w.acctOK(account) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	a, ok := w.accounts[account]
	if !ok {
		a = &RawAccount{AccountID: account}
		w.accounts[account] = a
	}
	// AccountSummary tags are not delivered in deterministic order. Prefer
	// the currency reported alongside NetLiquidation as the canonical
	// account currency; fall back to the first non-empty value otherwise.
	switch {
	case tag == ibapi.NetLiquidation && currency != "":
		a.Currency = currency
	case a.Currency == "" && currency != "":
		a.Currency = currency
	}
	switch tag {
	case ibapi.NetLiquidation:
		a.NetLiquidation = atof(value)
	case ibapi.TotalCashValue:
		a.TotalCash = atof(value)
	case ibapi.BuyingPower:
		a.BuyingPower = atof(value)
	}
}

// AccountSummaryEnd signals that all summary tags have been delivered.
func (w *gatherWrapper) AccountSummaryEnd(reqID int64) {
	w.summaryOnce.Do(func() { close(w.summaryDone) })
}

// Position records a single open position from the IB Gateway feed.
func (w *gatherWrapper) Position(account string, contract *ibapi.Contract, position ibapi.Decimal, avgCost float64) {
	if !w.acctOK(account) {
		return
	}
	if contract == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.positions = append(w.positions, RawPosition{
		Account:     account,
		Symbol:      contract.Symbol,
		SecType:     contract.SecType,
		Exchange:    contract.Exchange,
		Currency:    contract.Currency,
		Quantity:    position.Float(),
		AvgCost:     avgCost,
		ConID:       contract.ConID,
		LocalSymbol: contract.LocalSymbol,
		Strike:      contract.Strike,
		Expiry:      contract.LastTradeDateOrContractMonth,
		Right:       contract.Right,
		Multiplier:  contractMultiplier(contract.Multiplier),
	})
}

// PositionEnd signals that all positions have been delivered.
func (w *gatherWrapper) PositionEnd() {
	w.posOnce.Do(func() { close(w.posDone) })
}

// ExecDetails records one execution (fill) from the IB Gateway feed.
func (w *gatherWrapper) ExecDetails(reqID int64, contract *ibapi.Contract, execution *ibapi.Execution) {
	if contract == nil || execution == nil {
		return
	}
	if !w.acctOK(execution.AcctNumber) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.executions = append(w.executions, RawExecution{
		ExecID:      execution.ExecID,
		Account:     execution.AcctNumber,
		Symbol:      contract.Symbol,
		SecType:     contract.SecType,
		Exchange:    execution.Exchange,
		Currency:    contract.Currency,
		Side:        execution.Side,
		Shares:      execution.Shares.Float(),
		Price:       execution.Price,
		Time:        execution.Time,
		ConID:       contract.ConID,
		LocalSymbol: contract.LocalSymbol,
		Strike:      contract.Strike,
		Expiry:      contract.LastTradeDateOrContractMonth,
		Right:       contract.Right,
		Multiplier:  contractMultiplier(contract.Multiplier),
	})
}

// ExecDetailsEnd signals that all executions have been delivered.
func (w *gatherWrapper) ExecDetailsEnd(reqID int64) {
	w.execOnce.Do(func() { close(w.execDone) })
}

// Error is invoked for any IB-level error. We surface it to whichever wait
// is currently blocked.
func (w *gatherWrapper) Error(reqID int64, errorTime int64, errCode int64, errString string, advancedOrderRejectJSON string) {
	// IB Gateway sends a lot of informational "errors" with code < 2100 as
	// well as connection-status pings; only escalate genuine failures.
	if errCode < 2100 {
		select {
		case w.errCh <- fmt.Errorf("ibkr error %d: %s", errCode, errString):
		default:
		}
	}
}

// parseIBTime parses IB's execution time format "YYYYMMDD HH:MM:SS" (or
// variants like "YYYYMMDD-HH:MM:SS") into a UTC time.Time. A trailing zone
// name ("US/Eastern", "America/New_York", ...) is honored when it resolves.
// The second return is false when nothing parses: callers must reject the
// record, never stamp time.Now() (a moving date rewrites history on every
// sync).
// usZoneOffsets is the explicit offset policy for US market timezone
// abbreviations. time.Parse would otherwise accept any alphabetic token at
// a fabricated zero offset ("EST" → UTC), silently shifting executions by
// hours. Only listed abbreviations resolve; everything else is rejected.
var usZoneOffsets = map[string]int{
	"EST": -5 * 3600,
	"EDT": -4 * 3600,
	"CST": -6 * 3600,
	"CDT": -5 * 3600,
	"MST": -7 * 3600,
	"MDT": -6 * 3600,
	"PST": -8 * 3600,
	"PDT": -7 * 3600,
}

func parseIBTime(s string) (time.Time, bool) {
	// A trailing slash-form zone ("US/Eastern") must resolve explicitly:
	// falling through to abbreviation parsing would stamp a zero offset.
	if i := strings.LastIndex(s, " "); i > 0 {
		if zone := strings.TrimSpace(s[i+1:]); strings.Contains(zone, "/") {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				return time.Time{}, false
			}
			t, err := time.ParseInLocation("20060102 15:04:05", strings.TrimSpace(s[:i]), loc)
			if err != nil {
				return time.Time{}, false
			}
			return t.UTC(), true
		}
		if offset, ok := usZoneOffsets[strings.ToUpper(strings.TrimSpace(s[i+1:]))]; ok {
			if t, err := time.ParseInLocation("20060102 15:04:05", strings.TrimSpace(s[:i]), time.FixedZone("", offset)); err == nil {
				return t.UTC(), true
			}
			return time.Time{}, false
		}
		// Trailing alphabetic token that is neither a slash-form zone nor
		// a recognized abbreviation: reject rather than zero-offset it.
		if isAlphaToken(strings.TrimSpace(s[i+1:])) {
			return time.Time{}, false
		}
	}
	layouts := []string{
		"20060102 15:04:05",
		"20060102-15:04:05",
		"2006-01-02 15:04:05",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// isAlphaToken reports whether s looks like a zone abbreviation (letters
// only), as opposed to a malformed datetime.
func isAlphaToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

// contractMultiplier parses IB's contract multiplier ("100" for standard
// equity options, "" for stocks). Empty or invalid values mean 1: amounts
// stay unscaled rather than zeroed.
func contractMultiplier(s string) float64 {
	if s == "" {
		return 1
	}
	if m, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && m > 0 {
		return m
	}
	return 1
}

// optionSide maps IB's Right field to the domain option side.
func optionSide(right string) brokerage.OptionSide {
	switch strings.ToUpper(strings.TrimSpace(right)) {
	case "C", "CALL":
		return brokerage.OptionCall
	case "P", "PUT":
		return brokerage.OptionPut
	default:
		return ""
	}
}

// parseOptionExpiry parses IB's LastTradeDateOrContractMonth ("YYYYMMDD"
// for options). Unparseable values yield the zero time (unknown expiry,
// never today).
func parseOptionExpiry(s string) time.Time {
	if t, err := time.Parse("20060102", strings.TrimSpace(s)); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// occSymbol renders an OCC-style contract descriptor
// ("AAPL  260116C00250000") when IB omits the local symbol, so option
// series stay distinguishable by RawSymbol.
func occSymbol(underlying, expiry, right string, strike float64) string {
	side := "C"
	if optionSide(right) == brokerage.OptionPut {
		side = "P"
	}
	exp := strings.TrimSpace(expiry)
	if len(exp) == 8 {
		exp = exp[2:]
	}
	return fmt.Sprintf("%-6s%s%s%08d", underlying, exp, side, int64(math.Round(strike*1000)))
}

// optionPosition maps one OPT position row into the domain's option
// holdings. No market price arrives on the position feed, so Price stays
// zero (unknown); AvgCost is IB's reported per-unit average cost.
func optionPosition(p RawPosition) brokerage.OptionPosition {
	sym := formatSymbol(p.Symbol, p.Exchange, p.Currency)
	mult := p.Multiplier
	if mult <= 0 {
		mult = 1
	}
	return brokerage.OptionPosition{
		OptionSymbol: brokerage.OptionSymbol{
			Ticker:         sym,
			OptionType:     optionSide(p.Right),
			StrikePrice:    p.Strike,
			ExpirationDate: parseOptionExpiry(p.Expiry),
			IsMiniOption:   mult != 100,
			Underlying: brokerage.Symbol{
				Symbol:    sym,
				RawSymbol: p.Symbol,
				Type:      brokerage.SymbolType{Code: "EQUITY", IsSupported: true},
				Exchange:  brokerage.Exchange{Code: p.Exchange},
				Currency:  brokerage.Currency{Code: p.Currency},
			},
		},
		Units:                p.Quantity,
		AveragePurchasePrice: p.AvgCost,
		Currency:             brokerage.Currency{Code: p.Currency},
	}
}

// ibSecTypeToCode maps IB security type strings to canonical type codes.
func ibSecTypeToCode(secType string) string {
	switch secType {
	case "OPT":
		return "OPTION"
	case "FUT":
		return "FUTURE"
	case "CASH":
		return "FOREX"
	case "BOND":
		return "BOND"
	default:
		return "EQUITY"
	}
}

// formatSymbol appends a market suffix to the raw IB symbol when needed.
// For Hong Kong stocks (exchange SEHK or currency HKD), appends ".HK".
func formatSymbol(symbol, exchange, currency string) string {
	if exchange == "SEHK" || currency == "HKD" {
		return symbol + ".HK"
	}
	return symbol
}

func atof(s string) float64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64) //nolint:errcheck // exchange returns numeric strings; treat unparsable as zero
	return v
}
