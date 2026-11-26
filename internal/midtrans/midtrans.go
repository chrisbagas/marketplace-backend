// Package midtrans adalah klien tipis untuk Midtrans Snap + Status API.
//
// Alurnya: server membuat transaksi Snap (CreateSnap) → browser membuka
// popup Snap dengan token itu → Midtrans memberi tahu hasilnya lewat webhook
// DAN bisa dicek kapan saja lewat Status API. Keputusan "lunas/tidak" selalu
// diambil dari Status API (dipanggil server dengan Server Key), bukan dari
// isi request yang masuk, jadi webhook palsu tidak bisa melunasi pesanan.
package midtrans

import (
	"bytes"
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	SandboxSnapURL    = "https://app.sandbox.midtrans.com"
	SandboxAPIURL     = "https://api.sandbox.midtrans.com"
	ProductionSnapURL = "https://app.midtrans.com"
	ProductionAPIURL  = "https://api.midtrans.com"
)

type Client struct {
	ServerKey  string
	ClientKey  string
	SnapURL    string // basis Snap (juga sumber snap.js)
	APIURL     string // basis Core/Status API
	Production bool
	HTTP       *http.Client
}

func New(serverKey, clientKey string, production bool) *Client {
	c := &Client{ServerKey: serverKey, ClientKey: clientKey, Production: production,
		SnapURL: SandboxSnapURL, APIURL: SandboxAPIURL, HTTP: &http.Client{Timeout: 15 * time.Second}}
	if production {
		c.SnapURL, c.APIURL = ProductionSnapURL, ProductionAPIURL
	}
	return c
}

// ---- Snap ---------------------------------------------------------------

type Item struct {
	ID       string `json:"id"`
	Name     string `json:"name"` // maks 50 karakter di Midtrans
	Price    int    `json:"price"`
	Quantity int    `json:"quantity"`
}

type Customer struct {
	Name, Email, Phone, Address, City, Postal string
}

type SnapRequest struct {
	OrderID         string // unik per percobaan bayar, maks 50 karakter
	GrossAmount     int    // harus = jumlah price*quantity semua Items
	Customer        Customer
	Items           []Item
	ExpiryMinutes   int
	EnabledPayments []string // kosong = semua yang aktif di dashboard Midtrans
	FinishURL       string
}

type SnapResponse struct {
	Token       string `json:"token"`
	RedirectURL string `json:"redirect_url"`
}

func trunc(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func (c *Client) CreateSnap(ctx context.Context, r SnapRequest) (*SnapResponse, error) {
	sum := 0
	items := make([]Item, 0, len(r.Items))
	for _, it := range r.Items {
		sum += it.Price * it.Quantity
		it.Name, it.ID = trunc(it.Name, 50), trunc(it.ID, 50)
		items = append(items, it)
	}
	if sum != r.GrossAmount {
		return nil, fmt.Errorf("total item (%d) tidak sama dengan gross_amount (%d)", sum, r.GrossAmount)
	}
	body := map[string]any{
		"transaction_details": map[string]any{"order_id": r.OrderID, "gross_amount": r.GrossAmount},
		"item_details":        items,
		"customer_details": map[string]any{
			"first_name": trunc(r.Customer.Name, 50), "email": r.Customer.Email, "phone": r.Customer.Phone,
			"shipping_address": map[string]any{
				"first_name": trunc(r.Customer.Name, 50), "phone": r.Customer.Phone, "address": trunc(r.Customer.Address, 200),
				"city": trunc(r.Customer.City, 50), "postal_code": r.Customer.Postal, "country_code": "IDN",
			},
		},
	}
	if r.ExpiryMinutes > 0 {
		body["expiry"] = map[string]any{"unit": "minutes", "duration": r.ExpiryMinutes}
	}
	if len(r.EnabledPayments) > 0 {
		body["enabled_payments"] = r.EnabledPayments
	}
	if r.FinishURL != "" {
		body["callbacks"] = map[string]any{"finish": r.FinishURL}
	}
	var out SnapResponse
	if err := c.do(ctx, http.MethodPost, c.SnapURL+"/snap/v1/transactions", body, &out); err != nil {
		return nil, err
	}
	if out.Token == "" {
		return nil, errors.New("Midtrans tidak mengembalikan token")
	}
	return &out, nil
}

// ---- Notification & Status API -----------------------------------------------

type Notification struct {
	OrderID           string `json:"order_id"`
	TransactionID     string `json:"transaction_id"`
	TransactionStatus string `json:"transaction_status"`
	StatusCode        string `json:"status_code"`
	GrossAmount       string `json:"gross_amount"` // mis. "382000.00"
	SignatureKey      string `json:"signature_key"`
	PaymentType       string `json:"payment_type"`
	FraudStatus       string `json:"fraud_status"`
	Store             string `json:"store"`
	Acquirer          string `json:"acquirer"`
	VANumbers         []struct {
		Bank     string `json:"bank"`
		VANumber string `json:"va_number"`
	} `json:"va_numbers"`
}

// VerifySignature: SHA512(order_id + status_code + gross_amount + server_key).
func (c *Client) VerifySignature(n *Notification) bool {
	sum := sha512.Sum512([]byte(n.OrderID + n.StatusCode + n.GrossAmount + c.ServerKey))
	want := hex.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(n.SignatureKey))) == 1
}

// ErrNotFound: Midtrans belum punya transaksi untuk order_id ini (belum ada
// percobaan bayar di popup).
var ErrNotFound = errors.New("transaksi belum ada di Midtrans")

// Status membaca status transaksi langsung dari Midtrans.
func (c *Client) Status(ctx context.Context, orderID string) (*Notification, error) {
	var n Notification
	if err := c.do(ctx, http.MethodGet, c.APIURL+"/v2/"+url.PathEscape(orderID)+"/status", nil, &n); err != nil {
		return nil, err
	}
	if n.StatusCode == "404" {
		return nil, ErrNotFound
	}
	return &n, nil
}

// CheckCredentials memastikan Server Key diterima Midtrans (di lingkungan yang
// dipilih) dengan meminta status transaksi yang pasti tidak ada: kunci yang
// benar dijawab "404 transaksi tidak ada", kunci salah/lingkungan keliru dijawab 401.
// Galat jaringan dikembalikan apa adanya (bukan bukti kunci salah).
func (c *Client) CheckCredentials(ctx context.Context) error {
	_, err := c.Status(ctx, "kk-cek-kunci-tidak-ada")
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// AmountRupiah mengubah "382000.00" → 382000.
func (n *Notification) AmountRupiah() (int, error) {
	f, err := strconv.ParseFloat(n.GrossAmount, 64)
	if err != nil {
		return 0, fmt.Errorf("gross_amount tidak valid: %q", n.GrossAmount)
	}
	return int(f + 0.5), nil
}

// Method memberi label metode pembayaran yang ramah: "QRIS", "GoPay", "VA BCA", ...
func (n *Notification) Method() string {
	switch n.PaymentType {
	case "qris":
		return "QRIS"
	case "gopay":
		return "GoPay"
	case "shopeepay":
		return "ShopeePay"
	case "dana":
		return "DANA"
	case "credit_card":
		return "Kartu"
	case "echannel":
		return "VA Mandiri"
	case "permata":
		return "VA Permata"
	case "bank_transfer":
		if len(n.VANumbers) > 0 {
			return "VA " + strings.ToUpper(n.VANumbers[0].Bank)
		}
		return "Transfer bank"
	case "cstore":
		if n.Store != "" {
			return strings.ToUpper(n.Store[:1]) + n.Store[1:] // indomaret → Indomaret
		}
		return "Minimarket"
	}
	if n.PaymentType != "" {
		return n.PaymentType
	}
	return "belum dipilih"
}

// ---- HTTP -------------------------------------------------------------------

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("Midtrans %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, rawURL string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.ServerKey, "")
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 400 {
		var e struct {
			Messages []string `json:"error_messages"`
			Message  string   `json:"status_message"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Message
		if len(e.Messages) > 0 {
			msg = strings.Join(e.Messages, "; ")
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: res.StatusCode, Message: truncateMsg(msg)}
	}
	return json.Unmarshal(raw, out)
}

func truncateMsg(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
