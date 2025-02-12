package bitso

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	debug = false
)

const (
	apiPrefix      = "https://stage.bitso.com/api/"
	apiVersion     = "v3"
	defaultTickets = 1
)

// DefaultClient is the default Bitso API client to use.
var DefaultClient = NewClient(http.DefaultClient)

var (
	// Burst rate is disabled by default
	defaultBurstRate = time.Second * 0
)

// NewClient creates and returns a new Bitso API client.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	c := &Client{
		client:  httpClient,
		tickets: make(chan struct{}, defaultTickets),

		version:   apiVersion,
		burstRate: defaultBurstRate,
	}
	for i := 0; i < defaultTickets; i++ {
		c.tickets <- struct{}{}
	}
	return c
}

// A Client is a Bitso API consumer
type Client struct {
	client *http.Client

	apiPrefix string

	key     string
	secret  string
	version string

	tickets chan struct{}

	burstRate time.Duration
}

// SetAPIKey sets the user key to use for private API calls.
func (c *Client) SetAPIKey(key string) {
	c.key = key
}

// SetAPISecret sets the user secret to use for private API calls.
func (c *Client) SetAPISecret(secret string) {
	c.secret = secret
}

func (c *Client) endpointURL(endpoint string) (*url.URL, error) {
	return url.Parse(c.APIPrefix() + c.version + endpoint)
}

// SetAPIPrefix sets the API prefix
func (c *Client) SetAPIPrefix(prefix string) {
	if prefix != "" {
		prefix = strings.TrimRight(prefix, "/") + "/"
	}
	c.apiPrefix = prefix
}

// APIPrefix returns the API prefix
func (c *Client) APIPrefix() string {
	if c.apiPrefix != "" {
		return c.apiPrefix
	}
	return apiPrefix
}

func (c *Client) debugf(f string, a ...interface{}) {
	if !debug {
		return
	}
	log.Printf(f, a...)
}

func (c *Client) newRequest(method string, uri string, body io.Reader) (*http.Request, error) {
	nonce := time.Now().UnixNano()

	var buf []byte

	if body != nil {
		var err error
		buf, err = ioutil.ReadAll(body)
		if err != nil {
			return nil, err
		}
	}

	c.debugf("req: %v", string(buf))

	req, err := http.NewRequest(method, uri, bytes.NewBuffer(buf))
	if err != nil {
		return nil, err
	}

	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
	}

	if c.key == "" && c.secret == "" {
		// Return unsigned request
		return req, nil
	}

	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	message := fmt.Sprintf("%d%s%s%s", nonce, method, u.RequestURI(), string(buf))
	c.debugf("message: %v", message)

	mac := hmac.New(sha256.New, []byte(c.secret))
	mac.Write([]byte(message))
	signature := fmt.Sprintf("%x", mac.Sum(nil))

	authHeader := fmt.Sprintf("Bitso %s:%d:%s", c.key, nonce, signature)
	req.Header.Set("Authorization", authHeader)

	return req, err
}

func (c *Client) doRequest(method string, endpoint string, params url.Values, body io.Reader, dest interface{}) error {
	u, err := c.endpointURL(endpoint)
	if err != nil {
		return err
	}
	u.RawQuery = params.Encode()

	req, err := c.newRequest(method, u.String(), body)
	if err != nil {
		return err
	}

	// Apply burst-rate protection.
	if br := c.burstRate; br > 0 {
		<-c.tickets
		ticker := time.NewTicker(br)
		go func() {
			<-ticker.C
			c.tickets <- struct{}{}
		}()
	}

	res, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	c.debugf("res: %#v", res)
	c.debugf("res: %v", string(buf))

	var env Envelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return err
	}
	c.debugf("env: %#v", env)
	if !env.Success {
		code := fmt.Sprintf("%v", env.Error.Code)
		i, _ := strconv.Atoi(code)
		return apiError(i, env.Error.Message)
	}

	if err := json.Unmarshal(buf, dest); err != nil {
		return err
	}

	return nil
}

func (c *Client) deleteResponse(endpoint string, params url.Values, dest interface{}) error {
	return c.doRequest("DELETE", endpoint, params, nil, dest)
}

func (c *Client) getResponse(endpoint string, params url.Values, dest interface{}) error {
	return c.doRequest("GET", endpoint, params, nil, dest)
}

func (c *Client) postResponse(endpoint string, body interface{}, dest interface{}) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.doRequest("POST", endpoint, nil, bytes.NewBuffer(buf), dest)
}

// AvailableBooks returns a list of existing exchange order books and their
// respective order placement limits.
func (c *Client) AvailableBooks() ([]ExchangeOrderBook, error) {
	res := struct {
		Payload []ExchangeOrderBook `json:"payload"`
	}{}
	if err := c.getResponse("/available_books", nil, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// Tickers returns trading information from all books.
func (c *Client) Tickers() ([]Ticker, error) {
	res := struct {
		Payload []Ticker `json:"payload"`
	}{}
	if err := c.getResponse("/ticker", nil, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// Ticker returns trading information from the specified book.
func (c *Client) Ticker(book *Book) (*Ticker, error) {
	params := url.Values{
		"book": {book.String()},
	}
	res := struct {
		Payload Ticker `json:"payload"`
	}{}
	if err := c.getResponse("/ticker", params, &res); err != nil {
		return nil, err
	}
	return &res.Payload, nil
}

// Trades returns a list of recent trades from the specified book.
func (c *Client) Trades(params url.Values) ([]Trade, error) {
	res := struct {
		Payload []Trade `json:"payload"`
	}{}
	if err := c.getResponse("/trades", params, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// OrderBook returns a list of all open orders in the specified book.
func (c *Client) OrderBook(params url.Values) (*OrderBook, error) {
	res := struct {
		Payload OrderBook `json:"payload"`
	}{}
	if err := c.getResponse("/order_book", params, &res); err != nil {
		return nil, err
	}
	return &res.Payload, nil
}

// Balances returns information concerning the user’s balances for all supported
// currencies.
func (c *Client) Balances(params url.Values) ([]Balance, error) {
	res := struct {
		Payload struct {
			Balances []Balance `json:"balances"`
		} `json:"payload"`
	}{}
	if err := c.getResponse("/balance", params, &res); err != nil {
		return nil, err
	}
	return res.Payload.Balances, nil
}

// Fees returns information on customer fees for all available order books,
// and withdrawal fees for applicable currencies.
func (c *Client) Fees(params url.Values) (*CustomerFees, error) {
	res := struct {
		Payload CustomerFees `json:"payload"`
	}{}
	if err := c.getResponse("/fees", params, &res); err != nil {
		return nil, err
	}
	return &res.Payload, nil
}

// Ledger returns a list of all the user's registered operations.
func (c *Client) Ledger(params url.Values) ([]Transaction, error) {
	res := struct {
		Payload []Transaction `json:"payload"`
	}{}
	if err := c.getResponse("/ledger", params, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// LedgerByOperation returns a list of all the user's registered operations.
func (c *Client) LedgerByOperation(op Operation, params url.Values) ([]Transaction, error) {
	optype := map[Operation]string{
		OperationFunding:    "fundings",
		OperationWithdrawal: "withdrawals",
		OperationTrade:      "trades",
		OperationFee:        "fees",
	}
	res := struct {
		Payload []Transaction `json:"payload"`
	}{}
	if err := c.getResponse("/ledger/"+optype[op], params, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// Fundings returns detailed info on a user's fundings.
func (c *Client) Fundings(params url.Values) ([]Funding, error) {
	res := struct {
		Payload []Funding `json:"payload"`
	}{}
	if err := c.getResponse("/fundings", params, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// Withdrawals returns detailed info on user's withdrawals
func (c *Client) Withdrawals(params url.Values) ([]Withdrawal, error) {
	res := struct {
		Payload []Withdrawal `json:"payload"`
	}{}
	if err := c.getResponse("/withdrawals", params, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// MyTrades returns a list of the user's trades.
func (c *Client) MyTrades(params url.Values) ([]UserTrade, error) {
	res := struct {
		Payload []UserTrade `json:"payload"`
	}{}
	if err := c.getResponse("/user_trades", params, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// OrderTrades returns a list of the user's order trades on a given order.
func (c *Client) OrderTrades(oid string, params url.Values) ([]UserOrderTrade, error) {
	res := struct {
		Payload []UserOrderTrade `json:"payload"`
	}{}
	if err := c.getResponse("/order_trades/"+oid, params, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// MyOpenOrders a list of the user's open orders.
func (c *Client) MyOpenOrders(params url.Values) ([]UserOrder, error) {
	res := struct {
		Payload []UserOrder `json:"payload"`
	}{}
	if err := c.getResponse("/open_orders", params, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// LookupOrder returns details of an order given its order ID.
func (c *Client) LookupOrder(oid string) (*UserOrder, error) {
	orders, err := c.LookupOrders([]string{oid})
	if err != nil {
		return nil, err
	}
	if len(orders) > 0 {
		return &orders[0], nil
	}
	return nil, errors.New("no such order")
}

// LookupOrders returns a list of details for 1 or more orders
func (c *Client) LookupOrders(oids []string) ([]UserOrder, error) {
	res := struct {
		Payload []UserOrder `json:"payload"`
	}{}
	if err := c.getResponse("/orders/"+strings.Join(oids, "-"), nil, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// CancelOrders cancels open order(s)
func (c *Client) CancelOrders(oids []string) ([]string, error) {
	var res struct {
		Payload []string `json:"payload"`
	}
	if err := c.deleteResponse("/orders/"+strings.Join(oids, "-"), nil, &res); err != nil {
		return nil, err
	}
	return res.Payload, nil
}

// CancelOrder cancels an open order
func (c *Client) CancelOrder(oid string) ([]string, error) {
	return c.CancelOrders([]string{oid})
}

// PlaceOrder places a buy or sell order (both limit and market orders are
// available)
func (c *Client) PlaceOrder(order *OrderPlacement) (string, error) {
	var res struct {
		Payload struct {
			OID string `json:"oid"`
		} `json:"payload"`
	}
	if err := c.postResponse("/orders/", order, &res); err != nil {
		return "", err
	}
	return res.Payload.OID, nil
}

// Withdraw places a buy or sell order (both limit and market orders are
// available)
func (c *Client) Withdraw(withdrawal *WithdrawalPlacement) (string, error) {
	var res struct {
		Payload Withdrawal `json:"payload"`
	}
	if err := c.postResponse("/withdrawals/", withdrawal, &res); err != nil {
		return "", err
	}
	return res.Payload.WID, nil
}

// BurstRate returns the current burst-rate limit.
func (c *Client) BurstRate() time.Duration {
	return c.burstRate
}

// SetBurstRate sets the amount of time the client should wait in between
// requests.
func (c *Client) SetBurstRate(burstRate time.Duration) {
	c.burstRate = burstRate
}
