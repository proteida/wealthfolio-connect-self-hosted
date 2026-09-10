package okx

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/domain/brokerage"
	"github.com/wealthfolio/wealthfolio-connect-self-hosted/internal/infrastructure/clients/cexcommon"
)

type transactionParty struct {
	Address string `json:"address"`
	Amount  string `json:"amount"`
}

type web3Transaction struct {
	ChainIndex  string             `json:"chainIndex"`
	Hash        string             `json:"txHash"`
	Tier        string             `json:"itype"`
	Time        string             `json:"txTime"`
	From        []transactionParty `json:"from"`
	To          []transactionParty `json:"to"`
	Token       string             `json:"tokenContractAddress"`
	Amount      string             `json:"amount"`
	Symbol      string             `json:"symbol"`
	Fee         string             `json:"txFee"`
	Status      string             `json:"txStatus"`
	Blacklisted bool               `json:"hitBlacklist"`
}

type transactionPage struct {
	Cursor string `json:"cursor"`
	// The official field table and example use different names; accept both.
	Transactions    []web3Transaction `json:"transactions"`
	TransactionList []web3Transaction `json:"transactionList"`
}

func (c *Web3Client) fetchTransactions(ctx context.Context, w Wallet) ([]brokerage.Activity, error) {
	var all []brokerage.Activity
	seen := make(map[string]bool)
	tracked := make(map[string]bool)
	for _, wallet := range c.wallets {
		tracked[canonicalAddress(wallet.Address)] = true
	}
	end := strconv.FormatInt(c.now().UnixMilli(), 10)
	for chains := range slices.Chunk(w.Chains, 50) {
		// The live API rejects multi-chain limits above 20 (81001), despite
		// the larger limit in its docs. Use the accepted limit for every batch.
		limit := "20"
		query := url.Values{"address": {w.Address}, "chains": {strings.Join(chains, ",")}, "limit": {limit}, "end": {end}}
		cursors := make(map[string]bool)
		for {
			pages, err := web3Get[transactionPage](ctx, c, "/api/v6/dex/post-transaction/transactions-by-address", query)
			if err != nil {
				return all, err
			}
			if len(pages) == 0 {
				break
			}
			if len(pages) != 1 {
				return all, fmt.Errorf("okx_web3: unexpected history page count %d", len(pages))
			}
			page := pages[0]
			for _, tx := range append(page.Transactions, page.TransactionList...) {
				if !slices.Contains(chains, tx.ChainIndex) {
					continue
				}
				act, mapErr := transactionActivity(w.Address, tx, tracked)
				if mapErr != nil {
					return all, mapErr
				}
				if act != nil && !seen[act.ID] {
					all = append(all, *act)
					seen[act.ID] = true
				}
			}
			if page.Cursor == "" {
				break
			}
			if cursors[page.Cursor] {
				return all, fmt.Errorf("okx_web3: history cursor did not advance")
			}
			cursors[page.Cursor] = true
			query.Set("cursor", page.Cursor)
		}
	}
	return all, nil
}

// transactionActivity preserves swap legs as asset transfers grouped by hash.
// This endpoint supplies neither execution prices nor log indexes: do not invent
// USD cost basis or classify arbitrary contract calls as priced BUY/SELL trades.
// Legs touching only tracked wallets stay internal; any untracked counterparty
// flags the leg external (metadata.flow.is_external).
func transactionActivity(address string, tx web3Transaction, tracked map[string]bool) (*brokerage.Activity, error) {
	if tx.Status != "success" || tx.Blacklisted {
		return nil, nil
	}
	from, fromAmount, err := partyAmount(tx.From, address)
	if err != nil {
		return nil, err
	}
	to, toAmount, err := partyAmount(tx.To, address)
	if err != nil {
		return nil, err
	}
	if !from && !to {
		return nil, nil
	}
	amount, err := finiteAmount(tx.Amount)
	if err != nil {
		return nil, err
	}
	// Party amounts are authoritative for UTXO/multi-recipient transactions.
	if fromAmount != nil || toAmount != nil {
		amount = 0
		if toAmount != nil {
			amount += *toAmount
		}
		if fromAmount != nil {
			amount -= *fromAmount
		}
		if (from && fromAmount == nil) || (to && toAmount == nil) {
			return nil, fmt.Errorf("okx_web3: incomplete party amounts for %s", tx.Hash)
		}
	} else {
		if from && to {
			return nil, nil
		} // self-transfer, no asset movement
		if from {
			amount = -amount
		}
	}
	if amount == 0 {
		return nil, nil
	}
	timestamp, err := strconv.ParseInt(tx.Time, 10, 64)
	if err != nil || timestamp <= 0 || tx.Hash == "" || tx.ChainIndex == "" || tx.Symbol == "" {
		return nil, fmt.Errorf("okx_web3: invalid transaction identity/time for %s", tx.Hash)
	}
	typ := brokerage.ActivityTransferIn
	if amount < 0 {
		typ = brokerage.ActivityTransferOut
	}
	// Fingerprint immutable transfer content; omit time, status, fee and page
	// position so overlapping pages and later syncs produce the same ID.
	identity := struct {
		Chain, Hash, Tier, Token, Amount string
		From, To                         []string
	}{
		tx.ChainIndex, canonicalAddress(tx.Hash), tx.Tier, canonicalAddress(tx.Token),
		strconv.FormatFloat(math.Abs(amount), 'g', -1, 64), partyKeys(tx.From), partyKeys(tx.To),
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return nil, fmt.Errorf("okx_web3 transaction identity: %w", err)
	}
	key := fmt.Sprintf("%x", sha256.Sum256(raw))
	accountID := walletAccountID(address)
	id := accountID + ":" + key
	description := "On-chain asset transfer; historical valuation unavailable"
	if tx.Fee != "" {
		description += "; transaction network fee (native units): " + tx.Fee
	}
	return &brokerage.Activity{
		ID: id, AccountID: accountID, SourceRecordID: key,
		SourceGroupID: tx.ChainIndex + ":" + tx.Hash, ExternalReferenceID: tx.Hash,
		Type: typ, RawType: "TRANSFER_" + tx.Tier, Units: math.Abs(amount),
		IsExternal: hasExternalCounterparty(tx, address, tracked),
		TradeDate:  time.UnixMilli(timestamp).UTC(), Currency: brokerage.Currency{Code: "USD"},
		Symbol: &brokerage.Symbol{Symbol: cexcommon.NormalizeAsset(tx.Symbol), RawSymbol: cexcommon.NormalizeAsset(tx.Symbol),
			Type:     brokerage.SymbolType{Code: "CRYPTO", IsSupported: true},
			Exchange: brokerage.Exchange{Code: chainName(tx.ChainIndex)}, Currency: brokerage.Currency{Code: "USD"}},
		ProviderType: "okx_web3", SourceSystem: "okx_web3", NeedsReview: true, Description: description,
	}, nil
}

func finiteAmount(value string) (float64, error) {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, fmt.Errorf("okx_web3: invalid amount %q", value)
	}
	return n, nil
}

func partyAmount(parties []transactionParty, address string) (matched bool, quantity *float64, resultErr error) {
	found, supplied, missing := false, false, false
	var sum float64
	for _, party := range parties {
		for _, candidate := range strings.Split(party.Address, ",") {
			if canonicalAddress(strings.TrimSpace(candidate)) != canonicalAddress(address) {
				continue
			}
			found = true
			if party.Amount == "" {
				missing = true
				break
			}
			n, err := finiteAmount(party.Amount)
			if err != nil {
				return false, nil, err
			}
			sum += n
			supplied = true
			break
		}
	}
	if supplied && missing {
		return false, nil, fmt.Errorf("okx_web3: incomplete transaction party amounts")
	}
	if supplied {
		return found, &sum, nil
	}
	return found, nil, nil
}

// hasExternalCounterparty reports whether any party besides the wallet
// itself is outside the tracked set. Comma-joined addresses expand like
// partyKeys; transfers between tracked wallets stay internal.
func hasExternalCounterparty(tx web3Transaction, address string, tracked map[string]bool) bool {
	self := canonicalAddress(address)
	for _, party := range append(tx.From, tx.To...) {
		for _, raw := range strings.Split(party.Address, ",") {
			counterparty := canonicalAddress(strings.TrimSpace(raw))
			if counterparty == "" || counterparty == self {
				continue
			}
			if !tracked[counterparty] {
				return true
			}
		}
	}
	return false
}

func partyKeys(parties []transactionParty) []string {
	keys := make([]string, 0, len(parties))
	for _, party := range parties {
		addresses := strings.Split(party.Address, ",")
		for i, address := range addresses {
			addresses[i] = canonicalAddress(strings.TrimSpace(address))
		}
		sort.Strings(addresses)
		keys = append(keys, strings.Join(addresses, ","))
	}
	sort.Strings(keys)
	return keys
}
