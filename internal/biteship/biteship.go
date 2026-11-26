// Package biteship is a thin client for the Biteship shipping API.
//
// Endpoints used (https://biteship.com/id/docs/api/...):
//
//	GET  /v1/maps/areas       area search for the checkout autocomplete   (maps/search_area)
//	POST /v1/rates/couriers   live courier prices by area id              (rates/retrieve)
//	POST /v1/orders           book a shipment, returns the waybill        (orders/create)
//	GET  /v1/orders/{id}      order incl. waybill and status              (orders/retrieve)
//	GET  /v1/trackings/{id}   tracking status + history                   (trackings/retrieve)
//
// Authentication is the API key in the Authorization header. Webhooks are not
// signed: callers must authenticate them separately and always confirm the
// state with Tracking() before acting on it.
package biteship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const BaseURL = "https://api.biteship.com"

// codeDuplicateReference: POST /v1/orders with a reference_id that was already used.
const codeDuplicateReference = 40002060

// Tracking statuses (https://biteship.com/id/docs/api/trackings/status) that matter to us.
const (
	StatusPicked    = "picked"
	StatusDelivered = "delivered"
)

type Client struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
}

func New(apiKey string) *Client {
	return &Client{APIKey: apiKey, BaseURL: BaseURL, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// APIError is a non-2xx answer from Biteship.
type APIError struct {
	Status  int
	Code    int
	Message string
	Details map[string]any
}

func (e *APIError) Error() string {
	return fmt.Sprintf("biteship: HTTP %d (kode %d): %s", e.Status, e.Code, e.Message)
}

// DuplicateOrder reports whether err means "reference_id already used" and, if
// so, returns the existing order id and waybill (waybill may be empty).
func DuplicateOrder(err error) (orderID, waybill string, ok bool) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != codeDuplicateReference {
		return "", "", false
	}
	orderID, _ = apiErr.Details["order_id"].(string)
	waybill, _ = apiErr.Details["waybill_id"].(string)
	return orderID, waybill, orderID != ""
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.APIKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		apiErr := &APIError{Status: res.StatusCode, Message: strings.TrimSpace(string(raw))}
		var e struct {
			Code    int            `json:"code"`
			Error   string         `json:"error"`
			Details map[string]any `json:"details"`
		}
		if json.Unmarshal(raw, &e) == nil {
			apiErr.Code, apiErr.Details = e.Code, e.Details
			if e.Error != "" {
				apiErr.Message = e.Error
			}
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// ---- Maps ------------------------------------------------------------------

type Area struct {
	ID         string `json:"id"`
	Name       string `json:"name"` // "Pesanggrahan, Jakarta Selatan, DKI Jakarta. 12250"
	Province   string `json:"administrative_division_level_1_name"`
	City       string `json:"administrative_division_level_2_name"`
	District   string `json:"administrative_division_level_3_name"`
	PostalCode int    `json:"postal_code"`
}

// SearchAreas returns districts matching free text (Indonesia only).
func (c *Client) SearchAreas(ctx context.Context, input string) ([]Area, error) {
	q := url.Values{"countries": {"ID"}, "input": {input}, "type": {"single"}}
	var out struct {
		Areas []Area `json:"areas"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/maps/areas?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Areas, nil
}

// ---- Rates -----------------------------------------------------------------

type Item struct {
	Name     string `json:"name"`
	Value    int    `json:"value"`
	Quantity int    `json:"quantity"`
	Weight   int    `json:"weight"` // gram, per unit
	Category string `json:"category,omitempty"`
}

type RateRequest struct {
	OriginAreaID string
	DestAreaID   string
	Couriers     string // comma separated company codes, e.g. "jne,sicepat"
	Items        []Item
}

type Rate struct {
	Company      string `json:"courier_code"`
	CompanyName  string `json:"courier_name"`
	Type         string `json:"courier_service_code"`
	TypeName     string `json:"courier_service_name"`
	Duration     string `json:"shipment_duration_range"` // "2 - 3"
	DurationUnit string `json:"shipment_duration_unit"`  // days | hours
	Price        int    `json:"price"`
}

func (c *Client) Rates(ctx context.Context, r RateRequest) ([]Rate, error) {
	body := map[string]any{
		"origin_area_id":      r.OriginAreaID,
		"destination_area_id": r.DestAreaID,
		"couriers":            r.Couriers,
		"items":               r.Items,
	}
	var out struct {
		Pricing []Rate `json:"pricing"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/rates/couriers", body, &out); err != nil {
		return nil, err
	}
	return out.Pricing, nil
}

// ---- Orders ----------------------------------------------------------------

type Contact struct {
	Name, Phone, Email, Address, Note string
	AreaID                            string
	PostalCode                        string
}

type OrderRequest struct {
	ReferenceID string // our order id; Biteship enforces uniqueness → idempotent booking
	Origin      Contact
	Destination Contact
	Company     string
	Type        string
	Note        string
	Items       []Item
}

type Order struct {
	ID         string
	TrackingID string
	WaybillID  string
	Company    string
	Type       string
	Price      int
	Status     string
}

// orderPayload matches the JSON of POST /v1/orders and GET /v1/orders/{id}.
type orderPayload struct {
	ID      string `json:"id"`
	Price   int    `json:"price"`
	Status  string `json:"status"`
	Courier struct {
		TrackingID string `json:"tracking_id"`
		WaybillID  string `json:"waybill_id"`
		Company    string `json:"company"`
		Type       string `json:"type"`
	} `json:"courier"`
}

func (p orderPayload) order() *Order {
	return &Order{ID: p.ID, TrackingID: p.Courier.TrackingID, WaybillID: p.Courier.WaybillID,
		Company: p.Courier.Company, Type: p.Courier.Type, Price: p.Price, Status: p.Status}
}

func postal(s string) any {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return nil
	}
	return n
}

// CreateOrder books a pickup. delivery_type "now": the waybill is created immediately.
func (c *Client) CreateOrder(ctx context.Context, r OrderRequest) (*Order, error) {
	body := map[string]any{
		"reference_id":              r.ReferenceID,
		"origin_contact_name":       r.Origin.Name,
		"origin_contact_phone":      r.Origin.Phone,
		"origin_address":            r.Origin.Address,
		"origin_area_id":            r.Origin.AreaID,
		"destination_contact_name":  r.Destination.Name,
		"destination_contact_phone": r.Destination.Phone,
		"destination_address":       r.Destination.Address,
		"destination_area_id":       r.Destination.AreaID,
		"courier_company":           r.Company,
		"courier_type":              r.Type,
		"delivery_type":             "now",
		"items":                     r.Items,
	}
	set := func(k, v string) {
		if v != "" {
			body[k] = v
		}
	}
	set("origin_note", r.Origin.Note)
	set("origin_contact_email", r.Origin.Email)
	set("destination_contact_email", r.Destination.Email)
	set("destination_note", r.Destination.Note)
	set("order_note", r.Note)
	if p := postal(r.Origin.PostalCode); p != nil {
		body["origin_postal_code"] = p
	}
	if p := postal(r.Destination.PostalCode); p != nil {
		body["destination_postal_code"] = p
	}
	var out orderPayload
	if err := c.do(ctx, http.MethodPost, "/v1/orders", body, &out); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, errors.New("biteship: respons order tanpa id")
	}
	return out.order(), nil
}

func (c *Client) GetOrder(ctx context.Context, id string) (*Order, error) {
	var out orderPayload
	if err := c.do(ctx, http.MethodGet, "/v1/orders/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return out.order(), nil
}

// ---- Tracking ----------------------------------------------------------------

type TrackingEvent struct {
	Note      string `json:"note"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
}

type Tracking struct {
	ID        string          `json:"id"`
	WaybillID string          `json:"waybill_id"`
	OrderID   string          `json:"order_id"`
	Status    string          `json:"status"`
	Link      string          `json:"link"`
	History   []TrackingEvent `json:"history"`
}

func (c *Client) Tracking(ctx context.Context, trackingID string) (*Tracking, error) {
	var out Tracking
	if err := c.do(ctx, http.MethodGet, "/v1/trackings/"+url.PathEscape(trackingID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
