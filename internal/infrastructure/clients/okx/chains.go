package okx

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const chainDiscoveryTTL = 15 * time.Minute

var unsupportedWalletChains = map[string]bool{
	"0":   true, // Bitcoin
	"501": true, // Solana
	"607": true, // TON
}

type chainCacheEntry struct {
	chains  []string
	history []string
	expires time.Time
}

type chainRow struct {
	ChainIndex string `json:"chainIndex"`
	APIName    string `json:"apiName"`
}

type web3Envelope[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []T    `json:"data"`
}

func web3Get[T any](ctx context.Context, c *Web3Client, path string, query url.Values) ([]T, error) {
	var env web3Envelope[T]
	if err := signedRequest(ctx, c.http, c.baseURL, http.MethodGet, path, query, nil, c.creds, &env); err != nil {
		return nil, fmt.Errorf("okx_web3 %s: %w", path, err)
	}
	if env.Code != "0" {
		return nil, fmt.Errorf("okx_web3 %s: api error %s: %s", path, env.Code, env.Msg)
	}
	return env.Data, nil
}

// resolveChains never broadens configured chains. Automatic discovery is EVM-only
// per OKX's address-active-chain API; other address formats need explicit chains.
//
// It returns two scopes: funded chains (current balances, for holdings) and
// history chains (every active chain, for transaction history). History must
// not follow funding state: after a full withdrawal the withdrawal itself
// lives on a chain with no current balance, and pruning it would declare an
// incomplete history complete.
func (c *Web3Client) resolveChains(ctx context.Context, w Wallet) (funded, history []string, err error) {
	configured := len(w.Chains) > 0
	if configured {
		out := append([]string(nil), w.Chains...)
		return out, out, nil
	}
	address := canonicalAddress(w.Address)
	if !configured && (len(address) != 42 || !strings.HasPrefix(address, "0x")) {
		return nil, nil, fmt.Errorf("okx_web3: automatic discovery requires an EVM address; configure chains for %s", w.Address)
	}
	if !configured {
		if _, err := hex.DecodeString(address[2:]); err != nil {
			return nil, nil, fmt.Errorf("okx_web3: invalid EVM address: %w", err)
		}
	}
	if _, err := hex.DecodeString(address[2:]); err != nil {
		return nil, nil, fmt.Errorf("okx_web3: invalid EVM address: %w", err)
	}
	c.chainMu.Lock()
	defer c.chainMu.Unlock()
	if cached, ok := c.chainCache[address]; ok && c.now().Before(cached.expires) {
		return append([]string(nil), cached.chains...), append([]string(nil), cached.history...), nil
	}
	support, err := web3Get[chainRow](ctx, c, "/api/v6/explorer/address/supported-chains", nil)
	if err != nil {
		return nil, nil, err
	}
	eligible := make(map[string]bool)
	for _, row := range support {
		if row.APIName == "address-active-chain" {
			eligible[row.ChainIndex] = true
		}
	}
	active, err := web3Get[chainRow](ctx, c, "/api/v6/explorer/address/address-active-chain", url.Values{"address": {w.Address}})
	if err != nil {
		return nil, nil, err
	}
	// Current English Wallet API docs use this same support endpoint for both
	// balances and transaction history (tx-history-api-chains).
	supported, err := web3Get[chainRow](ctx, c, "/api/v6/dex/balance/supported/chain", nil)
	if err != nil {
		return nil, nil, err
	}
	walletSupport := make(map[string]bool)
	for _, row := range supported {
		walletSupport[row.ChainIndex] = true
	}
	candidate := w
	candidate.Chains = nil
	seen := make(map[string]bool)
	for _, row := range active {
		if eligible[row.ChainIndex] && walletSupport[row.ChainIndex] && !unsupportedWalletChains[row.ChainIndex] && !seen[row.ChainIndex] {
			candidate.Chains = append(candidate.Chains, row.ChainIndex)
			seen[row.ChainIndex] = true
		}
	}
	historyScope := append([]string(nil), candidate.Chains...)
	// Probe token quantities, not USD values: unpriced and dust holdings are
	// still non-zero balances. This probe is part of chain discovery.
	tokens, err := c.fetchWallet(ctx, candidate)
	if err != nil {
		return nil, nil, fmt.Errorf("okx_web3 chain balance discovery: %w", err)
	}
	chains := nonzeroChains(candidate.Chains, tokens)
	c.chainCache[address] = chainCacheEntry{chains: chains, history: historyScope, expires: c.now().Add(chainDiscoveryTTL)}
	return append([]string(nil), chains...), historyScope, nil
}

func nonzeroChains(chains []string, tokens []Web3Token) []string {
	funded := make(map[string]bool)
	for _, token := range tokens {
		if token.Quantity > 0 {
			funded[token.ChainIndex] = true
		}
	}
	out := make([]string, 0, len(chains))
	for _, chain := range chains {
		if funded[chain] {
			out = append(out, chain)
		}
	}
	return out
}

func canonicalAddress(address string) string {
	if strings.HasPrefix(strings.ToLower(address), "0x") {
		return strings.ToLower(address)
	}
	return address // Base58 addresses are case-sensitive.
}

func walletAccountID(address string) string { return "okxweb3-" + canonicalAddress(address) }
