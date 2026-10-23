package tools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/finance"
)

const financeSummaryRows = 40

const financeFilters = `
	"start": {"type": "string", "description": "First day, YYYY-MM-DD. Default 30 days before end"},
	"end": {"type": "string", "description": "Last day, YYYY-MM-DD. Default today"},
	"account_id": {"type": "integer", "description": "Only transactions to or from this account, from finance_accounts"},
	"category": {"type": "string", "description": "Only this category (exact name, any case), or \"none\" for uncategorized"},
	"budget": {"type": "string", "description": "Only this budget (exact name, any case), or \"none\" for none"},
	"tag": {"type": "string", "description": "Only transactions with this tag (exact name, any case)"},
	"payee": {"type": "string", "description": "Only transactions whose source or destination account name contains this, such as a shop or employer"},
	"search": {"type": "string", "description": "Only transactions whose description or notes contain all these words"},
	"min_amount": {"type": "number", "description": "Only amounts of at least this much"},
	"max_amount": {"type": "number", "description": "Only amounts of at most this much"}`

func FinanceTools(client *finance.Client, allowed []string) Provider {
	all := financeTools(client)
	return Provider{
		Catalog: all,
		Tools: func(ctx context.Context) []Tool {
			if u, ok := users.FromContext(ctx); ok && slices.Contains(allowed, u.Name) {
				return all
			}
			return nil
		},
	}
}

func financeTools(client *finance.Client) []Tool {
	return []Tool{
		{
			Name:      "finance_accounts",
			Untrusted: true,
			Description: "List the user's accounts in their Firefly III bookkeeping, with balances and account_ids. " +
				"Use it for \"how much do I have\", net worth, or to find an account_id for the other finance tools.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"type": {"type": "string", "enum": ["asset", "liabilities", "expense", "revenue", "all"],
						"description": "asset (default) for the user's own bank, savings and cash accounts, liabilities for loans and debts, expense for payees, revenue for payers"},
					"date": {"type": "string", "description": "Show balances at the end of this day, YYYY-MM-DD. Default today"}
				}
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Type string `json:"type"`
					Date string `json:"date"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				switch args.Type {
				case "":
					args.Type = "asset"
				case "asset", "liabilities", "expense", "revenue", "all":
				default:
					return "", fmt.Errorf("type must be asset, liabilities, expense, revenue or all, not %q", args.Type)
				}
				date, err := parseDay("date", args.Date)
				if err != nil {
					return "", err
				}
				accounts, err := client.Accounts(ctx, args.Type, date)
				if err != nil {
					return "", err
				}
				if len(accounts) == 0 {
					return "No " + args.Type + " accounts.", nil
				}
				var b strings.Builder
				totals := map[string]*big.Rat{}
				for _, a := range accounts {
					b.WriteString(describeAccount(a) + "\n")
					if a.Active && (a.Type == "asset" || a.Type == "liabilities") {
						addAmount(totals, a.CurrencyCode, parseAmount(a.CurrentBalance))
					}
				}
				if len(totals) > 0 && args.Type != "expense" && args.Type != "revenue" {
					b.WriteString("\nTotal of the active accounts above: " + formatTotals(totals, ""))
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name:      "finance_transactions",
			Untrusted: true,
			Description: "List the user's bank transactions from their Firefly III bookkeeping, newest first, with " +
				"totals of everything that matched. Use it for questions like \"what did I spend at Delhaize last " +
				"month\" or \"when did I last pay rent\". For totals by category, payee or month, use finance_summary; " +
				"for one transaction in full, finance_transaction.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"type": {"type": "string", "enum": ["withdrawal", "deposit", "transfer"], "description": "Leave out for all"},` + financeFilters + `,
					"limit": {"type": "integer", "description": "Maximum number of transactions to show, 1 to 200. Default 50. Totals cover all matches"}
				}
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					financeArgs
					Limit int `json:"limit"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if args.Type != "" && !isFinanceType(args.Type) {
					return "", fmt.Errorf("type must be withdrawal, deposit or transfer, not %q", args.Type)
				}
				limit := 50
				if args.Limit > 0 {
					limit = min(args.Limit, 200)
				}
				found, err := args.scan(ctx, client)
				if err != nil {
					return "", err
				}
				if len(found.splits) == 0 {
					return fmt.Sprintf("No matching transactions from %s.", found.period()), nil
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%d matching transactions from %s", len(found.splits), found.period())
				if len(found.splits) > limit {
					fmt.Fprintf(&b, ", the newest %d shown", limit)
				}
				b.WriteString(".\nTotals: " + found.totals() + "\n")
				if note := found.note(); note != "" {
					b.WriteString(note + "\n")
				}
				b.WriteString("\n")
				for _, s := range found.splits[:min(limit, len(found.splits))] {
					b.WriteString(splitLine(s) + "\n")
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name:      "finance_summary",
			Untrusted: true,
			Description: "Add up the user's spending, income or transfers from their Firefly III bookkeeping over a " +
				"period, grouped by category, budget, payee, tag, month or own account, largest first (months in " +
				"order). Use it for questions like \"where does my money go\", \"how much did I spend on groceries " +
				"each month this year\" or \"who are my biggest expenses\".",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"group_by": {"type": "string", "enum": ["category", "budget", "payee", "tag", "month", "account"],
						"description": "account is the user's own account the money left or reached. A transaction with several tags counts under each"},
					"type": {"type": "string", "enum": ["withdrawal", "deposit", "transfer"], "description": "withdrawal (default) for spending, deposit for income"},` + financeFilters + `
				},
				"required": ["group_by"]
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					financeArgs
					GroupBy string `json:"group_by"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if args.Type == "" {
					args.Type = "withdrawal"
				}
				if !isFinanceType(args.Type) {
					return "", fmt.Errorf("type must be withdrawal, deposit or transfer, not %q", args.Type)
				}
				key, ok := groupKeys[args.GroupBy]
				if !ok {
					return "", fmt.Errorf("group_by must be category, budget, payee, tag, month or account, not %q", args.GroupBy)
				}
				found, err := args.scan(ctx, client)
				if err != nil {
					return "", err
				}
				if len(found.splits) == 0 {
					return fmt.Sprintf("No matching %ss from %s.", args.Type, found.period()), nil
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%ss from %s by %s, %d transactions.\n", capitalize(args.Type), found.period(), args.GroupBy, len(found.splits))
				if note := found.note(); note != "" {
					b.WriteString(note + "\n")
				}
				b.WriteString(summarize(found.splits, key, args.GroupBy == "month"))
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name:        "finance_transaction",
			Untrusted:   true,
			Description: "Show one transaction from the user's Firefly III bookkeeping in full, every split with its notes, tags, dates and references, by its transaction_id from finance_transactions.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {"transaction_id": {"type": "integer"}},
				"required": ["transaction_id"]
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Id int64 `json:"transaction_id"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				if args.Id <= 0 {
					return "", errors.New("transaction_id is required")
				}
				g, err := client.Transaction(ctx, strconv.FormatInt(args.Id, 10))
				if err != nil {
					return "", err
				}
				return describeGroup(g), nil
			},
		},
	}
}

type financeArgs struct {
	Type      string   `json:"type"`
	Start     string   `json:"start"`
	End       string   `json:"end"`
	AccountId int64    `json:"account_id"`
	Category  string   `json:"category"`
	Budget    string   `json:"budget"`
	Tag       string   `json:"tag"`
	Payee     string   `json:"payee"`
	Search    string   `json:"search"`
	MinAmount *float64 `json:"min_amount"`
	MaxAmount *float64 `json:"max_amount"`
}

// split is one split with the transaction it belongs to, which is what the
// model asks finance_transaction for.
type split struct {
	finance.Split
	group  string
	title  string
	splits int
	amount *big.Rat
}

type scanned struct {
	start, end time.Time
	splits     []split
	complete   bool
}

func (s scanned) period() string {
	return s.start.Format(finance.DateFormat) + " to " + s.end.Format(finance.DateFormat)
}

func (s scanned) note() string {
	if s.complete {
		return ""
	}
	return fmt.Sprintf("Only the newest %d pages of the period were read, so older transactions are missing: narrow the dates for the rest.", finance.MaxPages)
}

func (s scanned) totals() string {
	byType := map[string]map[string]*big.Rat{}
	var types []string
	for _, sp := range s.splits {
		if byType[sp.Type] == nil {
			byType[sp.Type] = map[string]*big.Rat{}
			types = append(types, sp.Type)
		}
		addAmount(byType[sp.Type], sp.CurrencyCode, sp.amount)
	}
	slices.Sort(types)
	var parts []string
	for _, t := range types {
		parts = append(parts, t+"s "+formatTotals(byType[t], sign(t)))
	}
	return strings.Join(parts, "; ")
}

func (a financeArgs) scan(ctx context.Context, client *finance.Client) (scanned, error) {
	end, err := parseDay("end", a.End)
	if err != nil {
		return scanned{}, err
	}
	if end.IsZero() {
		end = time.Now()
	}
	start, err := parseDay("start", a.Start)
	if err != nil {
		return scanned{}, err
	}
	if start.IsZero() {
		start = end.AddDate(0, 0, -30)
	}
	if start.After(end) {
		return scanned{}, errors.New("start must not be after end")
	}
	q := finance.Query{Start: start, End: end, Type: a.Type}
	if a.AccountId > 0 {
		q.AccountId = strconv.FormatInt(a.AccountId, 10)
	}
	page, err := client.Transactions(ctx, q)
	if err != nil {
		return scanned{}, err
	}
	out := scanned{start: start, end: end, complete: page.Complete}
	words := strings.Fields(strings.ToLower(a.Search))
	for _, g := range page.Groups {
		for _, s := range g.Splits {
			sp := split{Split: s, group: g.Id, title: g.Title, splits: len(g.Splits), amount: parseAmount(s.Amount)}
			if a.matches(sp, words) {
				out.splits = append(out.splits, sp)
			}
		}
	}
	slices.SortStableFunc(out.splits, func(x, y split) int { return y.Date.Compare(x.Date) })
	return out, nil
}

func (a financeArgs) matches(s split, words []string) bool {
	if a.Type != "" && s.Type != a.Type {
		return false
	}
	if !matchName(a.Category, s.CategoryName) || !matchName(a.Budget, s.BudgetName) {
		return false
	}
	if a.Tag != "" && !slices.ContainsFunc(s.Tags, func(t string) bool { return strings.EqualFold(strings.TrimSpace(t), strings.TrimSpace(a.Tag)) }) {
		return false
	}
	if p := strings.ToLower(strings.TrimSpace(a.Payee)); p != "" &&
		!strings.Contains(strings.ToLower(s.SourceName), p) && !strings.Contains(strings.ToLower(s.DestinationName), p) {
		return false
	}
	text := strings.ToLower(s.Description + " " + s.Notes + " " + s.title)
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	amount, _ := s.amount.Float64()
	if a.MinAmount != nil && amount < *a.MinAmount {
		return false
	}
	if a.MaxAmount != nil && amount > *a.MaxAmount {
		return false
	}
	return true
}

func matchName(want, got string) bool {
	want = strings.TrimSpace(want)
	switch {
	case want == "":
		return true
	case strings.EqualFold(want, "none"):
		return got == ""
	}
	return strings.EqualFold(want, strings.TrimSpace(got))
}

var groupKeys = map[string]func(split) []string{
	"category": func(s split) []string { return []string{s.CategoryName} },
	"budget":   func(s split) []string { return []string{s.BudgetName} },
	"payee":    func(s split) []string { return []string{counterparty(s)} },
	"account":  func(s split) []string { return []string{ownAccount(s)} },
	"tag":      func(s split) []string { return s.Tags },
	"month":    func(s split) []string { return []string{s.Date.Format("2006-01")} },
}

func summarize(splits []split, key func(split) []string, chronological bool) string {
	type row struct {
		name  string
		total *big.Rat
		count int
	}
	type currency struct {
		total *big.Rat
		rows  map[string]*row
	}
	byCurrency := map[string]*currency{}
	for _, s := range splits {
		c := byCurrency[s.CurrencyCode]
		if c == nil {
			c = &currency{total: new(big.Rat), rows: map[string]*row{}}
			byCurrency[s.CurrencyCode] = c
		}
		c.total.Add(c.total, s.amount)
		names := key(s)
		if len(names) == 0 {
			names = []string{""}
		}
		for _, name := range names {
			name = oneLine(name)
			if name == "" {
				name = "(none)"
			}
			r := c.rows[name]
			if r == nil {
				r = &row{name: name, total: new(big.Rat)}
				c.rows[name] = r
			}
			r.total.Add(r.total, s.amount)
			r.count++
		}
	}
	codes := make([]string, 0, len(byCurrency))
	for code := range byCurrency {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	var b strings.Builder
	for _, code := range codes {
		c := byCurrency[code]
		rows := make([]*row, 0, len(c.rows))
		for _, r := range c.rows {
			rows = append(rows, r)
		}
		slices.SortFunc(rows, func(x, y *row) int {
			if chronological {
				return strings.Compare(x.name, y.name)
			}
			return cmp.Or(y.total.Cmp(x.total), strings.Compare(x.name, y.name))
		})
		fmt.Fprintf(&b, "\n%s, total %s:\n", code, c.total.FloatString(2))
		for i, r := range rows {
			if i == financeSummaryRows && len(rows) > financeSummaryRows+1 {
				rest, count := new(big.Rat), 0
				for _, r := range rows[i:] {
					rest.Add(rest, r.total)
					count += r.count
				}
				fmt.Fprintf(&b, "- %d others: %s (%s, %d transactions)\n", len(rows)-i, rest.FloatString(2), share(rest, c.total), count)
				break
			}
			fmt.Fprintf(&b, "- %s: %s (%s, %d transactions)\n", r.name, r.total.FloatString(2), share(r.total, c.total), r.count)
		}
	}
	return b.String()
}

func share(part, total *big.Rat) string {
	if total.Sign() == 0 {
		return "-"
	}
	f, _ := new(big.Rat).Quo(part, total).Float64()
	return fmt.Sprintf("%.1f%%", f*100)
}

func splitLine(s split) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s%s %s, %s → %s: %s", s.Date.Format(finance.DateFormat), sign(s.Type), s.amount.FloatString(2),
		s.CurrencyCode, oneLine(s.SourceName), oneLine(s.DestinationName), truncate(oneLine(s.Description), 120))
	var notes []string
	if s.Type != "withdrawal" && s.Type != "deposit" {
		notes = append(notes, s.Type)
	}
	if s.ForeignAmount != "" && s.ForeignCurrencyCode != "" {
		notes = append(notes, "paid "+parseAmount(s.ForeignAmount).FloatString(2)+" "+s.ForeignCurrencyCode)
	}
	if s.CategoryName != "" {
		notes = append(notes, "category: "+oneLine(s.CategoryName))
	}
	if s.BudgetName != "" {
		notes = append(notes, "budget: "+oneLine(s.BudgetName))
	}
	if len(s.Tags) > 0 {
		notes = append(notes, "tags: "+strings.Join(s.Tags, ", "))
	}
	if s.splits > 1 {
		notes = append(notes, fmt.Sprintf("one of %d splits", s.splits))
	}
	if len(notes) > 0 {
		b.WriteString(" (" + strings.Join(notes, "; ") + ")")
	}
	fmt.Fprintf(&b, " [transaction_id: %s]", s.group)
	return b.String()
}

func describeGroup(g finance.Group) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Transaction %s", g.Id)
	if g.Title != "" {
		b.WriteString(": " + oneLine(g.Title))
	}
	b.WriteString("\n")
	for i, s := range g.Splits {
		if len(g.Splits) > 1 {
			fmt.Fprintf(&b, "\nSplit %d of %d:\n", i+1, len(g.Splits))
		} else {
			b.WriteString("\n")
		}
		field := func(name, value string) {
			if value = strings.TrimSpace(value); value != "" {
				fmt.Fprintf(&b, "%s: %s\n", name, value)
			}
		}
		date := func(name string, t time.Time) {
			if !t.IsZero() {
				field(name, t.Format(finance.DateFormat))
			}
		}
		field("Description", s.Description)
		field("Type", s.Type)
		field("Date", s.Date.Format("2006-01-02 15:04 -07:00"))
		field("Amount", sign(s.Type)+parseAmount(s.Amount).FloatString(2)+" "+s.CurrencyCode)
		if s.ForeignAmount != "" {
			field("Foreign amount", parseAmount(s.ForeignAmount).FloatString(2)+" "+s.ForeignCurrencyCode)
		}
		field("From", accountRef(s.SourceName, s.SourceType, s.SourceIban, s.SourceId))
		field("To", accountRef(s.DestinationName, s.DestinationType, s.DestinationIban, s.DestinationId))
		field("Category", s.CategoryName)
		field("Budget", s.BudgetName)
		field("Subscription", cmp.Or(s.SubscriptionName, s.BillName))
		field("Tags", strings.Join(s.Tags, ", "))
		date("Book date", s.BookDate)
		date("Process date", s.ProcessDate)
		date("Interest date", s.InterestDate)
		date("Due date", s.DueDate)
		date("Payment date", s.PaymentDate)
		date("Invoice date", s.InvoiceDate)
		field("Internal reference", s.InternalReference)
		field("External id", s.ExternalId)
		field("External url", s.ExternalUrl)
		if s.Reconciled {
			field("Reconciled", "yes")
		}
		if s.HasAttachments {
			field("Attachments", "yes")
		}
		if notes := strings.TrimSpace(s.Notes); notes != "" {
			b.WriteString("Notes:\n" + notes + "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func accountRef(name, kind, iban, id string) string {
	s := oneLine(name)
	var extra []string
	if kind != "" {
		extra = append(extra, kind)
	}
	if iban != "" {
		extra = append(extra, iban)
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	if id != "" {
		s += " [account_id: " + id + "]"
	}
	return s
}

func describeAccount(a finance.Account) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [account_id: %s]: %s %s", oneLine(a.Name), a.Id, parseAmount(a.CurrentBalance).FloatString(2), a.CurrencyCode)
	var notes []string
	if role := cmp.Or(a.AccountRole, a.LiabilityType); role != "" {
		notes = append(notes, role)
	} else if a.Type != "" {
		notes = append(notes, a.Type)
	}
	if a.Iban != "" {
		notes = append(notes, a.Iban)
	}
	if !a.Active {
		notes = append(notes, "inactive")
	}
	if len(notes) > 0 {
		b.WriteString(" (" + strings.Join(notes, ", ") + ")")
	}
	return b.String()
}

// counterparty is the other side of a transaction: who was paid for a
// withdrawal, who paid for a deposit.
func counterparty(s split) string {
	if s.Type == "deposit" {
		return s.SourceName
	}
	return s.DestinationName
}

func ownAccount(s split) string {
	if s.Type == "deposit" {
		return s.DestinationName
	}
	return s.SourceName
}

func isFinanceType(t string) bool {
	return t == "withdrawal" || t == "deposit" || t == "transfer"
}

func sign(kind string) string {
	switch kind {
	case "withdrawal":
		return "-"
	case "deposit":
		return "+"
	}
	return ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func parseDay(name, value string) (time.Time, error) {
	if value = strings.TrimSpace(value); value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(finance.DateFormat, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a date like 2026-10-23, not %q", name, value)
	}
	return t, nil
}

func parseAmount(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return new(big.Rat)
	}
	return r
}

func addAmount(totals map[string]*big.Rat, currency string, amount *big.Rat) {
	if totals[currency] == nil {
		totals[currency] = new(big.Rat)
	}
	totals[currency].Add(totals[currency], amount)
}

func formatTotals(totals map[string]*big.Rat, prefix string) string {
	codes := make([]string, 0, len(totals))
	for code := range totals {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	parts := make([]string, len(codes))
	for i, code := range codes {
		parts[i] = prefix + totals[code].FloatString(2) + " " + code
	}
	return strings.Join(parts, ", ")
}
