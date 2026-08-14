package fintech

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// krakenWriteCapableMethodNames denylists exported KrakenSync methods that
// would let this package place orders, cancel orders, or move funds. If
// this test ever fails, it means someone added a write-capable method to
// KrakenSync — that must not happen without deliberately revisiting the
// read-only guarantee this sync path is built on.
var krakenWriteCapableMethodNames = []string{
	"AddOrder", "AddOrderBatch", "EditOrder", "CancelOrder", "CancelAll",
	"CancelAllOrdersAfter", "CancelOrderBatch", "Withdraw", "WithdrawFunds",
	"WithdrawCancel", "DepositAddress", "AddExport", "WalletTransfer",
}

func TestKrakenSync_ExposesNoWriteCapableMethods(t *testing.T) {
	syncType := reflect.TypeOf(&KrakenSync{})
	denylist := make(map[string]bool, len(krakenWriteCapableMethodNames))
	for _, n := range krakenWriteCapableMethodNames {
		denylist[n] = true
	}

	allowed := map[string]bool{"Balance": true, "TradesHistory": true, "Configured": true}
	for i := 0; i < syncType.NumMethod(); i++ {
		name := syncType.Method(i).Name
		if denylist[name] {
			t.Errorf("KrakenSync exposes write-capable method %q — this sync path must stay read-only", name)
		}
		if !allowed[name] {
			t.Errorf("unexpected exported KrakenSync method %q — keep the read-only surface minimal", name)
		}
	}
}

func TestKrakenSync_NotConfigured_NoRequestSent(t *testing.T) {
	requestSent := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSent = true
		fmt.Fprint(w, `{"error":[],"result":{}}`)
	}))
	defer ts.Close()

	original := krakenBaseURL
	krakenBaseURL = ts.URL
	t.Cleanup(func() { krakenBaseURL = original })

	k := NewKrakenSync("", "")
	if _, err := k.Balance(context.Background()); err != ErrKrakenNotConfigured {
		t.Errorf("Balance() error = %v, want ErrKrakenNotConfigured", err)
	}
	if _, err := k.TradesHistory(context.Background()); err != ErrKrakenNotConfigured {
		t.Errorf("TradesHistory() error = %v, want ErrKrakenNotConfigured", err)
	}
	if requestSent {
		t.Fatal("no request should have been sent for an unconfigured KrakenSync")
	}
}

func TestSignKrakenRequest_KnownVector(t *testing.T) {
	// Kraken's documented example secret/nonce/postdata, used by go_fintech's
	// signing test, must produce the same signature here since this is a port.
	const secret = "kQH5HW/8p1uGOVjbgWA7FunAmGgRq+CHE8LFjzNJ/0e8gtBWvixLm4PIcKj3wkSAxhi8C0fNXyvfQ0qPzmuPHA=="
	sig, err := signKrakenRequest(secret, "/0/private/AddOrder", 1616492376594,
		"nonce=1616492376594&ordertype=limit&pair=XBTUSD&price=37500&type=buy&volume=1.25")
	if err != nil {
		t.Fatalf("signKrakenRequest() error: %v", err)
	}
	// Independently computed in Python (matches go_fintech's fixture), so this
	// catches any drift in the ported HMAC-SHA512 signing implementation.
	const want = "lYZbMSJfR0TKtOvnHytss/wugRccX1vm2HyTNOTXac17Xd/ur36o2/fSQkOyenFXTyVNwJ1RUeQziKqX9xJqew=="
	if sig != want {
		t.Errorf("signKrakenRequest() = %q, want %q", sig, want)
	}
}

func TestSignKrakenRequest_NeverLeaksSecret(t *testing.T) {
	const fakeSecret = "not valid base64 !!! kQH5HW8p1uGOVjbgWA7Fun"
	_, err := signKrakenRequest(fakeSecret, "/0/private/Balance", 1, "nonce=1")
	if err == nil {
		t.Fatal("expected an error for invalid secret encoding")
	}
	if strings.Contains(err.Error(), fakeSecret) {
		t.Fatalf("signing error leaked the API secret: %v", err)
	}
}

func TestKrakenPairToSymbol(t *testing.T) {
	cases := map[string]string{
		"XXBTZUSD": "BTC",
		"XETHZUSD": "ETH",
		"XBTUSD":   "BTC",
		"ETHUSD":   "ETH",
		"SOLUSD":   "SOL",
	}
	for pair, want := range cases {
		if got, _ := krakenPairToSymbol(pair); got != want {
			t.Errorf("krakenPairToSymbol(%q) = %q, want %q", pair, got, want)
		}
	}
}
