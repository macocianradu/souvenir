// Package finance reads a Firefly III server's accounts and transactions
// through its REST API. It only ever sends GET requests.
package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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
	params := url.Values{"type": {kind}, "limit": {strconv.Itoa(pageSize)}}
	if !date.IsZero() {
		params.Set("date", date.Format(DateFormat))
	}
	var out []Account
	for page := 1; page <= MaxPages; page++ {
		params.Set("page", strconv.Itoa(page))
		var resp list[Account]
		if err := c.get(ctx, "/api/v1/accounts?"+params.Encode(), &resp); err != nil {
			return nil, err
		}
		for _, item := range resp.Data {
			item.Attributes.Id = item.Id
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
		if err := c.get(ctx, path+"?"+params.Encode(), &resp); err != nil {
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
	if err := c.get(ctx, "/api/v1/transactions/"+url.PathEscape(id), &resp); err != nil {
		return Group{}, err
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

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.url+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.api+json")
	req.Header.Set("Authorization", "Bearer "+c.token)
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
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("not found")
	default:
		c.logger.Error("Firefly III returned an error", "status", resp.Status, "body", strings.TrimSpace(string(data[:min(len(data), 512)])))
		return fmt.Errorf("firefly iii returned %s", resp.Status)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("could not read the firefly iii response: %w", err)
	}
	return nil
}
