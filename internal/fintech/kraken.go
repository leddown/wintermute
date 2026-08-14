// Read-only Kraken sync, ported from /home/l3d/go/go_fintech's
// internal/kraken package. It is intentionally read-only: KrakenSync
// exposes only Balance and TradesHistory — Kraken's query (non-mutating)
// private endpoints. It must never grow a method that places orders,
// cancels orders, or moves funds (AddOrder, CancelOrder, Withdraw, etc.);
// see security_test.go's denylist, which fails loudly if one is ever
// added. The Kraken API key configured for this app should also be
// scoped to query-only permissions in Kraken's UI as a second layer of
// defense. Kraken is a crypto-only exchange — this sync path never
// covers stocks/ETFs, only the crypto side of the ledger.
package fintech

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// krakenBaseURL is intentionally not derived from config or the
// environment, so it can never be redirected via configuration. It is a
// package-level var rather than a const only so tests can point it at an
// httptest.Server; there is no exported way to change it.
var krakenBaseURL = "https://api.kraken.com"

// ErrKrakenNotConfigured is returned by every KrakenSync method when no
// API credentials have been configured.
var ErrKrakenNotConfigured = errors.New("fintech: kraken API credentials not configured")

// KrakenBalances maps asset code (e.g. "ZUSD", "XXBT") to the available
// balance as a decimal string, exactly as returned by Kraken — never
// parsed into float64, to avoid rounding error on values with up to 8
// decimal places.
type KrakenBalances map[string]string

// KrakenTrade mirrors a single entry from Kraken's TradesHistory result.
// ID is populated from the response's map key (Kraken's trade ID).
type KrakenTrade struct {
	ID     string  `json:"id"`
	Pair   string  `json:"pair"`
	Time   float64 `json:"time"`
	Type   string  `json:"type"` // "buy" | "sell"
	Price  string  `json:"price"`
	Cost   string  `json:"cost"`
	Fee    string  `json:"fee"`
	Volume string  `json:"vol"`
}

// KrakenSync is a read-only Kraken REST API client used to import crypto
// trade history into the ledger. Do not add an exported method that
// calls an arbitrary path or any order-placing/fund-moving endpoint.
type KrakenSync struct {
	apiKey     string
	apiSecret  string
	httpClient *http.Client
	nonce      *krakenNonceSource
}

// NewKrakenSync builds a KrakenSync. apiKey/apiSecret may be empty; in
// that case every method returns ErrKrakenNotConfigured without making a
// request.
func NewKrakenSync(apiKey, apiSecret string) *KrakenSync {
	return &KrakenSync{
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		nonce:      newKrakenNonceSource(),
	}
}

// Configured reports whether both credential fields are set.
func (k *KrakenSync) Configured() bool {
	return k.apiKey != "" && k.apiSecret != ""
}

// Balance returns current account balances for all assets.
func (k *KrakenSync) Balance(ctx context.Context) (KrakenBalances, error) {
	raw, err := k.privatePOST(ctx, "/0/private/Balance", nil)
	if err != nil {
		return nil, err
	}
	var out KrakenBalances
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("fintech: decoding kraken Balance response: %w", err)
	}
	return out, nil
}

// TradesHistory returns the account's recent trade history, keyed by
// Kraken trade ID.
func (k *KrakenSync) TradesHistory(ctx context.Context) (map[string]KrakenTrade, error) {
	raw, err := k.privatePOST(ctx, "/0/private/TradesHistory", nil)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Trades map[string]KrakenTrade `json:"trades"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("fintech: decoding kraken TradesHistory response: %w", err)
	}
	for id, t := range wrapper.Trades {
		t.ID = id
		wrapper.Trades[id] = t
	}
	return wrapper.Trades, nil
}

// privatePOST signs and sends a request to a Kraken private endpoint and
// returns the raw "result" field from the response. path must start with
// "/0/private/" and is always a literal passed by one of the methods
// above — never derived from caller input.
func (k *KrakenSync) privatePOST(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	if !k.Configured() {
		return nil, ErrKrakenNotConfigured
	}

	if params == nil {
		params = url.Values{}
	}
	nonce := k.nonce.next()
	params.Set("nonce", strconv.FormatInt(nonce, 10))
	postData := params.Encode()

	sign, err := signKrakenRequest(k.apiSecret, path, nonce, postData)
	if err != nil {
		return nil, fmt.Errorf("fintech: signing kraken request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, krakenBaseURL+path, strings.NewReader(postData))
	if err != nil {
		return nil, fmt.Errorf("fintech: building kraken request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("API-Key", k.apiKey)
	req.Header.Set("API-Sign", sign)

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fintech: kraken request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("fintech: reading kraken response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fintech: unexpected kraken HTTP status %d", resp.StatusCode)
	}

	var envelope struct {
		Error  []string        `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("fintech: decoding kraken response envelope: %w", err)
	}
	if len(envelope.Error) > 0 {
		return nil, fmt.Errorf("fintech: kraken API error: %s", strings.Join(envelope.Error, "; "))
	}
	return envelope.Result, nil
}

// signKrakenRequest implements Kraken's documented signing algorithm:
// HMAC-SHA512(base64Decode(secret), path + SHA256(nonce + postData)),
// base64-encoded.
func signKrakenRequest(secret, path string, nonce int64, postData string) (string, error) {
	decodedSecret, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return "", fmt.Errorf("invalid API secret encoding: %w", err)
	}

	sha := sha256.New()
	sha.Write([]byte(strconv.FormatInt(nonce, 10) + postData))
	shaSum := sha.Sum(nil)

	var message bytes.Buffer
	message.WriteString(path)
	message.Write(shaSum)

	mac := hmac.New(sha512.New, decodedSecret)
	mac.Write(message.Bytes())
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// krakenNonceSource generates strictly increasing nonces, as required by
// Kraken's anti-replay check. A bare time.Now().UnixNano() call is not
// safe to use directly: two calls in quick succession (or a clock that
// doesn't advance between calls) could otherwise produce a
// non-increasing value.
type krakenNonceSource struct {
	mu   sync.Mutex
	last int64
}

func newKrakenNonceSource() *krakenNonceSource { return &krakenNonceSource{} }

func (n *krakenNonceSource) next() int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now().UnixNano()
	if now <= n.last {
		now = n.last + 1
	}
	n.last = now
	return now
}

// krakenPairBases maps Kraken's legacy asset codes to a plain ticker
// symbol. This is a pragmatic static table covering the most commonly
// traded assets rather than a call to Kraken's public AssetPairs
// endpoint (deferred — see the module plan's explicit deferrals).
var krakenPairBases = map[string]string{
	"XXBT": "BTC", "XBT": "BTC", "XETH": "ETH", "XLTC": "LTC", "XXRP": "XRP",
	"XXLM": "XLM", "XZEC": "ZEC", "XXMR": "XMR", "XETC": "ETC", "XREP": "REP",
	"XDOGE": "DOGE", "XXDG": "DOGE",
}

// krakenQuoteCodes are the recognized quote-currency suffixes, ordered
// longest-first so that a legacy code like "ZUSD" is matched before the
// plain "USD" it ends with — otherwise "XXBTZUSD" would strip "USD" and
// leave a stray "Z" on the base asset.
var krakenQuoteCodes = []string{"USDT", "USDC", "ZUSD", "ZEUR", "ZGBP", "ZCAD", "ZJPY", "USD", "EUR", "GBP"}

// krakenPairToSymbol resolves a Kraken pair like "XXBTZUSD" to a plain
// base-asset ticker symbol like "BTC". If pair doesn't match a known
// quote suffix, the raw pair is returned unchanged with ok=false — sync
// still proceeds (better than dropping the trade), just without a
// prettified symbol.
func krakenPairToSymbol(pair string) (symbol string, ok bool) {
	for _, quoteCode := range krakenQuoteCodes {
		if strings.HasSuffix(pair, quoteCode) && len(pair) > len(quoteCode) {
			base := pair[:len(pair)-len(quoteCode)]
			if mapped, found := krakenPairBases[base]; found {
				return mapped, true
			}
			return base, true
		}
	}
	return pair, false
}

// SyncKraken fetches the configured Kraken account's trade history and
// records each trade into the ledger (source=kraken_sync,
// asset_class=crypto), going through the same RecordTrade path as
// manual entry and CSV import so dedupe and validation behave
// identically. It is read-only against Kraken: it never places, cancels,
// or modifies an order there.
func (s *Service) SyncKraken(ctx context.Context) (ImportResult, error) {
	if s.kraken == nil || !s.kraken.Configured() {
		return ImportResult{}, ErrKrakenNotConfigured
	}

	trades, err := s.kraken.TradesHistory(ctx)
	if err != nil {
		return ImportResult{}, err
	}

	result := ImportResult{TotalRows: len(trades)}
	for id, t := range trades {
		symbol, _ := krakenPairToSymbol(t.Pair)
		priceCents, err := parseUnsignedCents(t.Price)
		if err != nil {
			result.Errors = append(result.Errors, RowError{Message: fmt.Sprintf("trade %s: invalid price: %v", id, err)})
			continue
		}
		feeCents, err := parseUnsignedCents(t.Fee)
		if err != nil {
			result.Errors = append(result.Errors, RowError{Message: fmt.Sprintf("trade %s: invalid fee: %v", id, err)})
			continue
		}

		_, err = s.RecordTrade(ctx, RecordTradeInput{
			Symbol:     symbol,
			AssetClass: AssetClassCrypto,
			Side:       Side(t.Type),
			Quantity:   t.Volume,
			PriceCents: priceCents,
			FeeCents:   feeCents,
			ExecutedAt: time.Unix(int64(t.Time), 0).UTC(),
			Source:     SourceKrakenSync,
			ExternalID: id,
		})
		switch {
		case errors.Is(err, ErrDuplicate):
			result.SkippedDuplicates++
		case err != nil:
			result.Errors = append(result.Errors, RowError{Message: fmt.Sprintf("trade %s: %v", id, err)})
		default:
			result.Inserted++
		}
	}
	return result, nil
}
