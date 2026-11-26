package midtrans

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Cancel and refund (Core API v2). Midtrans often answers HTTP 200 with the
// real result in the body's status_code, so every call checks that field.
//
//	Cancel: POST {api}/v2/{order_id}/cancel   — voids a pending / not yet settled transaction
//	Refund: POST {api}/v2/{order_id}/refund   — returns money for a settled transaction
//	        (the single refund endpoint; supported payment types: see RefundSupport)
//
// Docs: https://docs.midtrans.com/reference/cancel-transaction
//       https://docs.midtrans.com/reference/refund-transaction
//       https://docs.midtrans.com/reference/direct-refund-transaction

// Result is the common part of the cancel / refund responses.
type Result struct {
	StatusCode         string      `json:"status_code"`
	StatusMessage      string      `json:"status_message"`
	TransactionID      string      `json:"transaction_id"`
	OrderID            string      `json:"order_id"`
	PaymentType        string      `json:"payment_type"`
	TransactionStatus  string      `json:"transaction_status"`
	GrossAmount        string      `json:"gross_amount"`
	RefundAmount       string      `json:"refund_amount"`
	RefundKey          string      `json:"refund_key"`
	RefundChargebackID json.Number `json:"refund_chargeback_id"`
}

type RefundRequest struct {
	// RefundKey makes retries idempotent: Midtrans rejects a reused key with
	// "Duplicate refund ID". Letters, digits, '-' and '_' only.
	RefundKey string `json:"refund_key"`
	Amount    int    `json:"amount"`
	Reason    string `json:"reason,omitempty"`
}

// RefundSupport says whether Midtrans can refund a settled payment of this
// payment_type through the API, and whether it accepts a partial amount.
// Everything else (bank-transfer VA, Mandiri bill, Permata, Indomaret/Alfamart,
// direct debit, ...) can only be refunded by the merchant transferring the
// money by hand.
func RefundSupport(paymentType string) (supported, partial bool) {
	switch paymentType {
	case "credit_card", "gopay", "qris", "dana", "kredivo", "akulaku":
		return true, true
	case "shopeepay", "ovo": // Midtrans: no partial refund for these two
		return true, false
	}
	return false, false
}

// post sends a body-less or JSON POST and requires status_code 200.
func (c *Client) post(ctx context.Context, path string, in any) (*Result, error) {
	var res Result
	if err := c.do(ctx, http.MethodPost, c.APIURL+path, in, &res); err != nil {
		return nil, err
	}
	if res.StatusCode == "404" {
		return nil, ErrNotFound
	}
	if res.StatusCode != "200" {
		return &res, &APIError{Status: atoiOr(res.StatusCode, 502), Message: truncateMsg(res.StatusMessage)}
	}
	return &res, nil
}

func atoiOr(s string, def int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n == 0 {
		return def
	}
	return n
}

// Cancel voids a transaction that has not settled (pending VA / QRIS / e-wallet,
// or an authorised card payment). A settled one answers 412: use Refund.
func (c *Client) Cancel(ctx context.Context, orderID string) (*Result, error) {
	return c.post(ctx, "/v2/"+url.PathEscape(orderID)+"/cancel", nil)
}

// Refund returns money for a settled transaction.
func (c *Client) Refund(ctx context.Context, orderID string, r RefundRequest) (*Result, error) {
	res, err := c.post(ctx, "/v2/"+url.PathEscape(orderID)+"/refund", r)
	if err != nil {
		return res, err
	}
	if res.TransactionStatus != "refund" && res.TransactionStatus != "partial_refund" {
		return res, &APIError{Status: 502, Message: "status transaksi setelah refund: " + res.TransactionStatus}
	}
	return res, nil
}
