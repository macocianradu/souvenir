// Package finance reads a Firefly III server's accounts and transactions
// through its REST API. Its only writes are setting a transaction's category
// or subscription and creating a category or a subscription; it never edits
// or deletes anything else.
package finance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/config"
)

const (
	maxBody  = 16 << 20
	pageSize = 500
	MaxPages = 20
)

const DateFormat = "2006-01-02"

type Account struct {
	Id                 string    `json:"-"`
	Name               string    `json:"name"`
	Type               string    `json:"type"`
	AccountRole        string    `json:"account_role"`
	Active             bool      `json:"active"`
	CurrencyCode       string    `json:"currency_code"`
	CurrentBalance     string    `json:"current_balance"`
	CurrentBalanceDate time.Time `json:"current_balance_date"`
	Iban               string    `json:"iban"`
	LiabilityType      string    `json:"liability_type"`
	Notes              string    `json:"notes"`
}

type Split struct {
	JournalId           string    `json:"transaction_journal_id"`
	Type                string    `json:"type"`
	Date                time.Time `json:"date"`
	Amount              string    `json:"amount"`
	CurrencyCode        string    `json:"currency_code"`
	ForeignAmount       string    `json:"foreign_amount"`
	ForeignCurrencyCode string    `json:"foreign_currency_code"`
	Description         string    `json:"description"`
	SourceId            string    `json:"source_id"`
	SourceName          string    `json:"source_name"`
	SourceIban          string    `json:"source_iban"`
	SourceType          string    `json:"source_type"`
	DestinationId       string    `json:"destination_id"`
	DestinationName     string    `json:"destination_name"`
	DestinationIban     string    `json:"destination_iban"`
	DestinationType     string    `json:"destination_type"`
	CategoryName        string    `json:"category_name"`
	BudgetName          string    `json:"budget_name"`
	BillName            string    `json:"bill_name"`
	SubscriptionName    string    `json:"subscription_name"`
	Tags                []string  `json:"tags"`
	Notes               string    `json:"notes"`
	Reconciled          bool      `json:"reconciled"`
	InternalReference   string    `json:"internal_reference"`
	ExternalId          string    `json:"external_id"`
	ExternalUrl         string    `json:"external_url"`
	BookDate            time.Time `json:"book_date"`
	ProcessDate         time.Time `json:"process_date"`
	InterestDate        time.Time `json:"interest_date"`
	DueDate             time.Time `json:"due_date"`
	PaymentDate         time.Time `json:"payment_date"`
	InvoiceDate         time.Time `json:"invoice_date"`
	HasAttachments      bool      `json:"has_attachments"`
}

type Category struct {
	Id   string `json:"-"`
	Name string `json:"name"`
}

// Subscription is what Firefly III's API calls a bill: a recurring payment
// it expects within an amount range.
type Subscription struct {
	Id                string    `json:"-"`
	Name              string    `json:"name"`
	AmountMin         string    `json:"amount_min"`
	AmountMax         string    `json:"amount_max"`
	CurrencyCode      string    `json:"currency_code"`
	Date              time.Time `json:"date"`
	EndDate           time.Time `json:"end_date"`
	RepeatFreq        string    `json:"repeat_freq"`
	Skip              int       `json:"skip"`
	Active            bool      `json:"active"`
	Notes             string    `json:"notes"`
	NextExpectedMatch time.Time `json:"next_expected_match"`
}

type NewSubscription struct {
	Name         string `json:"name"`
	AmountMin    string `json:"amount_min"`
	AmountMax    string `json:"amount_max"`
	CurrencyCode string `json:"currency_code,omitempty"`
	Date         string `json:"date"`
	EndDate      string `json:"end_date,omitempty"`
	RepeatFreq   string `json:"repeat_freq"`
	Skip         int    `json:"skip"`
	Active       bool   `json:"active"`
	Notes        string `json:"notes,omitempty"`
}

// Assignment is what Assign sets on a transaction's splits; an empty id
// leaves that field as it is.
type Assignment struct {
	CategoryId     string
	SubscriptionId string
}

// Group is a Firefly transaction: one or more splits booked together.
type Group struct {
	Id     string  `json:"-"`
	Title  string  `json:"group_title"`
	Splits []Split `json:"transactions"`
}

type Query struct {
	Start     time.Time
	End       time.Time
	Type      string
	AccountId string
}

// Page is one scan's result: Complete is false when the period held more
// than MaxPages pages and the rest was not read.
type Page struct {
	Groups   []Group
	Complete bool
}

type Client struct {
	logger slog.Logger
	url    string
	token  string
	client *http.Client
}

func New(cfg config.FinanceConfig) *Client {
	return &Client{
		logger: *slog.Default().With("Component", "Finance"),
		url:    strings.TrimSuffix(cfg.Url, "/"),
		token:  cfg.Token,
		client: &http.Client{Timeout: time.Duration(cfg.Timeout) * time.Millisecond},
	}
}

func (c *Client) Accounts(ctx context.Context, kind string, date time.Time) ([]Account, error) {
	params := url.Values{"type": {kind}}
	if !date.IsZero() {
		params.Set("date", date.Format(DateFormat))
	}
	return all(ctx, c, "/api/v1/accounts", params, func(a *Account, id string) { a.Id = id })
}

func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	return all(ctx, c, "/api/v1/categories", url.Values{}, func(cat *Category, id string) { cat.Id = id })
}

func (c *Client) Subscriptions(ctx context.Context) ([]Subscription, error) {
	return all(ctx, c, "/api/v1/bills", url.Values{}, func(s *Subscription, id string) { s.Id = id })
}

// all reads every page of a list, up to MaxPages pages.
func all[T any](ctx context.Context, c *Client, path string, params url.Values, setId func(*T, string)) ([]T, error) {
	params.Set("limit", strconv.Itoa(pageSize))
	var out []T
	for page := 1; page <= MaxPages; page++ {
		params.Set("page", strconv.Itoa(page))
		var resp list[T]
		if err := c.do(ctx, "GET", path+"?"+params.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		for _, item := range resp.Data {
			setId(&item.Attributes, item.Id)
			out = append(out, item.Attributes)
		}
		if page >= resp.Meta.Pagination.TotalPages {
			break
		}
	}
	return out, nil
}

// Transactions reads every transaction in the query's period, newest first,
// up to MaxPages pages.
func (c *Client) Transactions(ctx context.Context, q Query) (Page, error) {
	path := "/api/v1/transactions"
	if q.AccountId != "" {
		path = "/api/v1/accounts/" + url.PathEscape(q.AccountId) + "/transactions"
	}
	params := url.Values{"limit": {strconv.Itoa(pageSize)}}
	if !q.Start.IsZero() {
		params.Set("start", q.Start.Format(DateFormat))
	}
	if !q.End.IsZero() {
		params.Set("end", q.End.Format(DateFormat))
	}
	if q.Type != "" {
		params.Set("type", q.Type)
	}
	var out Page
	for page := 1; page <= MaxPages; page++ {
		params.Set("page", strconv.Itoa(page))
		var resp list[Group]
		if err := c.do(ctx, "GET", path+"?"+params.Encode(), nil, &resp); err != nil {
			return Page{}, err
		}
		for _, item := range resp.Data {
			item.Attributes.Id = item.Id
			out.Groups = append(out.Groups, item.Attributes)
		}
		if page >= resp.Meta.Pagination.TotalPages {
			out.Complete = true
			break
		}
	}
	return out, nil
}

func (c *Client) Transaction(ctx context.Context, id string) (Group, error) {
	var resp struct {
		Data item[Group] `json:"data"`
	}
	if err := c.do(ctx, "GET", "/api/v1/transactions/"+url.PathEscape(id), nil, &resp); err != nil {
		return Group{}, err
	}
	resp.Data.Attributes.Id = resp.Data.Id
	return resp.Data.Attributes, nil
}

// Assign sets a category or subscription on the splits of g whose journal ids
// are in journals, and changes nothing else. Every split is named in the
// update, the others with their id alone, so Firefly III cannot take a split
// left out as one to remove. Rules are not run on the change.
func (c *Client) Assign(ctx context.Context, g Group, journals []string, a Assignment) (Group, error) {
	if a.CategoryId == "" && a.SubscriptionId == "" {
		return Group{}, errors.New("nothing to assign")
	}
	type splitUpdate struct {
		JournalId  string `json:"transaction_journal_id"`
		CategoryId string `json:"category_id,omitempty"`
		BillId     string `json:"bill_id,omitempty"`
	}
	update := struct {
		ApplyRules bool          `json:"apply_rules"`
		Splits     []splitUpdate `json:"transactions"`
	}{}
	for _, s := range g.Splits {
		u := splitUpdate{JournalId: s.JournalId}
		if slices.Contains(journals, s.JournalId) {
			u.CategoryId, u.BillId = a.CategoryId, a.SubscriptionId
		}
		update.Splits = append(update.Splits, u)
	}
	var resp struct {
		Data item[Group] `json:"data"`
	}
	if err := c.do(ctx, "PUT", "/api/v1/transactions/"+url.PathEscape(g.Id), update, &resp); err != nil {
		return Group{}, err
	}
	resp.Data.Attributes.Id = resp.Data.Id
	return resp.Data.Attributes, nil
}

func (c *Client) CreateCategory(ctx context.Context, name string) (Category, error) {
	var resp struct {
		Data item[Category] `json:"data"`
	}
	if err := c.do(ctx, "POST", "/api/v1/categories", Category{Name: name}, &resp); err != nil {
		return Category{}, err
	}
	resp.Data.Attributes.Id = resp.Data.Id
	return resp.Data.Attributes, nil
}

func (c *Client) CreateSubscription(ctx context.Context, s NewSubscription) (Subscription, error) {
	var resp struct {
		Data item[Subscription] `json:"data"`
	}
	if err := c.do(ctx, "POST", "/api/v1/bills", s, &resp); err != nil {
		return Subscription{}, err
	}
	resp.Data.Attributes.Id = resp.Data.Id
	return resp.Data.Attributes, nil
}

type item[T any] struct {
	Id         string `json:"id"`
	Attributes T      `json:"attributes"`
}

type list[T any] struct {
	Data []item[T] `json:"data"`
	Meta struct {
		Pagination struct {
			TotalPages int `json:"total_pages"`
		} `json:"pagination"`
	} `json:"meta"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("Request failed", "error", err)
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
	case http.StatusNotFound:
		return fmt.Errorf("not found")
	case http.StatusUnprocessableEntity:
		return validationError(data)
	default:
		c.logger.Error("Firefly III returned an error", "method", method, "status", resp.Status, "body", strings.TrimSpace(string(data[:min(len(data), 512)])))
		return fmt.Errorf("firefly iii returned %s", resp.Status)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("could not read the firefly iii response: %w", err)
	}
	return nil
}

// validationError turns Firefly III's 422 answer into the messages it gives
// for each field, so the model can correct its request.
func validationError(data []byte) error {
	var v struct {
		Message string              `json:"message"`
		Errors  map[string][]string `json:"errors"`
	}
	if json.Unmarshal(data, &v) != nil {
		return errors.New("firefly iii refused the request")
	}
	fields := slices.Sorted(maps.Keys(v.Errors))
	var parts []string
	for _, f := range fields {
		parts = append(parts, f+": "+strings.Join(v.Errors[f], " "))
	}
	if len(parts) == 0 {
		return fmt.Errorf("firefly iii refused the request: %s", v.Message)
	}
	return fmt.Errorf("firefly iii refused the request: %s", strings.Join(parts, "; "))
}
