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

// categorySeparator joins a category path: Firefly III categories are flat,
// so a subcategory is one named "Food / Groceries".
const categorySeparator = " / "

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
		{
			Name:      "finance_categories",
			Untrusted: true,
			Description: "List the categories in the user's Firefly III bookkeeping; a subcategory is named " +
				"\"Parent / Child\". Use it before finance_assign, which only takes a category that exists.",
			Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				categories, err := client.Categories(ctx)
				if err != nil {
					return "", err
				}
				if len(categories) == 0 {
					return "No categories.", nil
				}
				names := make([]string, len(categories))
				for i, c := range categories {
					names[i] = oneLine(c.Name)
				}
				slices.SortFunc(names, func(x, y string) int { return strings.Compare(strings.ToLower(x), strings.ToLower(y)) })
				return strings.Join(names, "\n"), nil
			},
		},
		{
			Name:      "finance_subscriptions",
			Untrusted: true,
			Description: "List the subscriptions (recurring bills) in the user's Firefly III bookkeeping, with their " +
				"expected amounts, how often they repeat and when they are next due. Check it before " +
				"finance_subscription_create so a subscription is not made twice.",
			Parameters: json.RawMessage(`{"type": "object", "properties": {}}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				subs, err := client.Subscriptions(ctx)
				if err != nil {
					return "", err
				}
				if len(subs) == 0 {
					return "No subscriptions.", nil
				}
				var b strings.Builder
				for _, s := range subs {
					b.WriteString(describeSubscription(s) + "\n")
				}
				return strings.TrimSpace(b.String()), nil
			},
		},
		{
			Name:      "finance_assign",
			Sensitive: true,
			Description: "Set the category, the subscription or both on transactions in the user's Firefly III " +
				"bookkeeping. It changes nothing else, only takes a category from finance_categories or a " +
				"subscription from finance_subscriptions, and never creates either (finance_category_create and " +
				"finance_subscription_create do). Use it when the user asks to " +
				"categorise transactions or link payments to a subscription; show them what you would change first " +
				"when they did not say exactly.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"transactions": {"type": "array", "maxItems": 50, "description": "The transactions to change, from finance_transactions",
						"items": {"type": "object", "properties": {
							"transaction_id": {"type": "integer"},
							"split": {"type": "integer", "description": "For a split transaction, the split number to change. Leave out for every split"}
						}, "required": ["transaction_id"]}},
					"category": {"type": "string", "description": "An existing category's name"},
					"subscription": {"type": "string", "description": "An existing subscription's name"}
				},
				"required": ["transactions"]
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Transactions []struct {
						Id    int64 `json:"transaction_id"`
						Split int   `json:"split"`
					} `json:"transactions"`
					Category     string `json:"category"`
					Subscription string `json:"subscription"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				switch {
				case len(args.Transactions) == 0:
					return "", errors.New("name at least one transaction")
				case len(args.Transactions) > 50:
					return "", errors.New("at most 50 transactions at a time")
				case strings.TrimSpace(args.Category) == "" && strings.TrimSpace(args.Subscription) == "":
					return "", errors.New("give a category, a subscription or both")
				}
				var a finance.Assignment
				var set []string
				if name := strings.TrimSpace(args.Category); name != "" {
					categories, err := client.Categories(ctx)
					if err != nil {
						return "", err
					}
					i := slices.IndexFunc(categories, func(c finance.Category) bool { return strings.EqualFold(strings.TrimSpace(c.Name), name) })
					if i < 0 {
						return "", fmt.Errorf("no category %q; list them with finance_categories, or create it first with finance_category_create", name)
					}
					a.CategoryId = categories[i].Id
					set = append(set, "category "+categories[i].Name)
				}
				if name := strings.TrimSpace(args.Subscription); name != "" {
					subs, err := client.Subscriptions(ctx)
					if err != nil {
						return "", err
					}
					i := slices.IndexFunc(subs, func(s finance.Subscription) bool { return strings.EqualFold(strings.TrimSpace(s.Name), name) })
					if i < 0 {
						return "", fmt.Errorf("no subscription %q; list them with finance_subscriptions", name)
					}
					a.SubscriptionId = subs[i].Id
					set = append(set, "subscription "+subs[i].Name)
				}
				var b strings.Builder
				changed := 0
				for _, t := range args.Transactions {
					line, err := assignOne(ctx, client, t.Id, t.Split, a)
					if err != nil {
						fmt.Fprintf(&b, "- transaction %d: not changed, %v\n", t.Id, err)
						continue
					}
					changed++
					b.WriteString("- " + line + "\n")
				}
				if changed == 0 {
					return "", errors.New("no transaction was changed:\n" + strings.TrimSpace(b.String()))
				}
				return fmt.Sprintf("Set %s on %d of %d transactions:\n%s", strings.Join(set, " and "), changed, len(args.Transactions), strings.TrimSpace(b.String())), nil
			},
		},
		{
			Name:      "finance_category_create",
			Sensitive: true,
			Description: "Create categories and subcategories in the user's Firefly III bookkeeping. Firefly III " +
				"categories are flat, so a subcategory is a category named \"Parent / Child\", and any parent in " +
				"the path that is missing is created too. It only creates; it cannot rename or delete one, and " +
				"skips a name that already exists. Check finance_categories first, and set them on transactions " +
				"with finance_assign.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"categories": {"type": "array", "maxItems": 50, "description": "The categories to create",
						"items": {"type": "object", "properties": {
							"name": {"type": "string", "description": "Such as \"Groceries\""},
							"parent": {"type": "string", "description": "For a subcategory, its parent's name, such as \"Food\" or \"Home / Utilities\". Leave out for a top-level category"}
						}, "required": ["name"]}}
				},
				"required": ["categories"]
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Categories []struct {
						Name   string `json:"name"`
						Parent string `json:"parent"`
					} `json:"categories"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				switch {
				case len(args.Categories) == 0:
					return "", errors.New("name at least one category")
				case len(args.Categories) > 50:
					return "", errors.New("at most 50 categories at a time")
				}
				var paths [][]string
				for _, c := range args.Categories {
					path, err := categoryPath(c.Parent, c.Name)
					if err != nil {
						return "", err
					}
					paths = append(paths, path)
				}
				categories, err := client.Categories(ctx)
				if err != nil {
					return "", err
				}
				// existing maps a lower-cased name to its spelling in Firefly III,
				// so a path under an existing category keeps that spelling.
				existing := map[string]string{}
				for _, c := range categories {
					name := strings.Join(splitCategory(c.Name), categorySeparator)
					existing[strings.ToLower(name)] = name
				}
				var created, skipped []string
				for _, path := range paths {
					name := ""
					for i, part := range path {
						if i > 0 {
							name += categorySeparator
						}
						name += part
						if spelled, ok := existing[strings.ToLower(name)]; ok {
							name = spelled
							if i == len(path)-1 && !slices.Contains(created, name) {
								skipped = append(skipped, name)
							}
							continue
						}
						if _, err := client.CreateCategory(ctx, name); err != nil {
							out := fmt.Sprintf("could not create %q: %v", name, err)
							if len(created) > 0 {
								out += "; created before that: " + strings.Join(created, ", ")
							}
							return "", errors.New(out)
						}
						existing[strings.ToLower(name)] = name
						created = append(created, name)
					}
				}
				var b strings.Builder
				if len(created) == 0 {
					b.WriteString("Created nothing.")
				} else {
					b.WriteString("Created:\n- " + strings.Join(created, "\n- "))
				}
				if len(skipped) > 0 {
					b.WriteString("\nAlready existed:\n- " + strings.Join(skipped, "\n- "))
				}
				return b.String(), nil
			},
		},
		{
			Name:      "finance_subscription_create",
			Sensitive: true,
			Description: "Create a subscription (a recurring bill such as rent, a phone plan or a streaming service) in " +
				"the user's Firefly III bookkeeping, so it can track whether each payment arrived. It only creates; " +
				"it cannot change or delete one. Check finance_subscriptions first, and base the amounts and dates " +
				"on the user's past payments from finance_transactions. Link the payments to it with finance_assign.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Such as \"Netflix\" or \"Rent\""},
					"amount_min": {"type": "number", "description": "The smallest expected payment"},
					"amount_max": {"type": "number", "description": "The largest expected payment. Default amount_min"},
					"currency_code": {"type": "string", "description": "Such as EUR. Default the user's main currency"},
					"date": {"type": "string", "description": "A day it is due, such as the first payment, YYYY-MM-DD; later ones follow from repeat_freq"},
					"repeat_freq": {"type": "string", "enum": ["weekly", "monthly", "quarterly", "half-year", "yearly"]},
					"skip": {"type": "integer", "description": "Periods skipped between payments: 1 with monthly is every other month. Default 0"},
					"end_date": {"type": "string", "description": "The last day it applies, YYYY-MM-DD, for one that ends"},
					"notes": {"type": "string"}
				},
				"required": ["name", "amount_min", "date", "repeat_freq"]
			}`),
			Run: func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Name         string   `json:"name"`
					AmountMin    float64  `json:"amount_min"`
					AmountMax    *float64 `json:"amount_max"`
					CurrencyCode string   `json:"currency_code"`
					Date         string   `json:"date"`
					RepeatFreq   string   `json:"repeat_freq"`
					Skip         int      `json:"skip"`
					EndDate      string   `json:"end_date"`
					Notes        string   `json:"notes"`
				}
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", err
				}
				args.Name = strings.TrimSpace(args.Name)
				if args.Name == "" {
					return "", errors.New("name is required")
				}
				amountMax := args.AmountMin
				if args.AmountMax != nil {
					amountMax = *args.AmountMax
				}
				if args.AmountMin <= 0 || amountMax < args.AmountMin {
					return "", errors.New("amount_min must be positive and amount_max at least amount_min")
				}
				switch args.RepeatFreq {
				case "weekly", "monthly", "quarterly", "half-year", "yearly":
				default:
					return "", fmt.Errorf("repeat_freq must be weekly, monthly, quarterly, half-year or yearly, not %q", args.RepeatFreq)
				}
				if args.Skip < 0 {
					return "", errors.New("skip must not be negative")
				}
				date, err := parseDay("date", args.Date)
				if err != nil || date.IsZero() {
					return "", cmp.Or(err, errors.New("date is required"))
				}
				end, err := parseDay("end_date", args.EndDate)
				if err != nil {
					return "", err
				}
				if !end.IsZero() && end.Before(date) {
					return "", errors.New("end_date must not be before date")
				}
				existing, err := client.Subscriptions(ctx)
				if err != nil {
					return "", err
				}
				if i := slices.IndexFunc(existing, func(s finance.Subscription) bool { return strings.EqualFold(strings.TrimSpace(s.Name), args.Name) }); i >= 0 {
					return "", fmt.Errorf("a subscription named %q already exists: %s", args.Name, describeSubscription(existing[i]))
				}
				s := finance.NewSubscription{
					Name:         args.Name,
					AmountMin:    strconv.FormatFloat(args.AmountMin, 'f', -1, 64),
					AmountMax:    strconv.FormatFloat(amountMax, 'f', -1, 64),
					CurrencyCode: strings.ToUpper(strings.TrimSpace(args.CurrencyCode)),
					Date:         date.Format(finance.DateFormat),
					RepeatFreq:   args.RepeatFreq,
					Skip:         args.Skip,
					Active:       true,
					Notes:        strings.TrimSpace(args.Notes),
				}
				if !end.IsZero() {
					s.EndDate = end.Format(finance.DateFormat)
				}
				created, err := client.CreateSubscription(ctx, s)
				if err != nil {
					return "", err
				}
				return "Created " + describeSubscription(created), nil
			},
		},
	}
}

// categoryPath is the category name under parent as its parts, each trimmed.
// Only " / " separates parts, so a name such as "Gas/Electricity" stays whole.
func categoryPath(parent, name string) ([]string, error) {
	path := append(splitCategory(parent), splitCategory(name)...)
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("every category needs a name")
	}
	for _, p := range path {
		if p == "" {
			return nil, fmt.Errorf("category %q has an empty part", strings.Join(path, categorySeparator))
		}
	}
	return path, nil
}

func splitCategory(name string) []string {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	parts := strings.Split(name, categorySeparator)
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
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
	index  int
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
		for i, s := range g.Splits {
			sp := split{Split: s, group: g.Id, title: g.Title, index: i + 1, splits: len(g.Splits), amount: parseAmount(s.Amount)}
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
	if s.SubscriptionName != "" || s.BillName != "" {
		notes = append(notes, "subscription: "+oneLine(cmp.Or(s.SubscriptionName, s.BillName)))
	}
	if len(notes) > 0 {
		b.WriteString(" (" + strings.Join(notes, "; ") + ")")
	}
	fmt.Fprintf(&b, " [transaction_id: %s", s.group)
	if s.splits > 1 {
		fmt.Fprintf(&b, ", split %d of %d", s.index, s.splits)
	}
	b.WriteString("]")
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

// assignOne applies a to one transaction, or to one of its splits, and
// describes what it changed.
func assignOne(ctx context.Context, client *finance.Client, id int64, splitNo int, a finance.Assignment) (string, error) {
	if id <= 0 {
		return "", errors.New("transaction_id is required")
	}
	g, err := client.Transaction(ctx, strconv.FormatInt(id, 10))
	if err != nil {
		return "", err
	}
	if len(g.Splits) == 0 {
		return "", errors.New("it has no splits")
	}
	targets := g.Splits
	if splitNo != 0 {
		if splitNo < 1 || splitNo > len(g.Splits) {
			return "", fmt.Errorf("it has %d splits, not a split %d", len(g.Splits), splitNo)
		}
		targets = g.Splits[splitNo-1 : splitNo]
	}
	journals := make([]string, len(targets))
	for i, s := range targets {
		journals[i] = s.JournalId
	}
	if _, err := client.Assign(ctx, g, journals, a); err != nil {
		return "", err
	}
	s := targets[0]
	line := fmt.Sprintf("transaction %d, %s %s%s %s to %s", id, s.Date.Format(finance.DateFormat), sign(s.Type),
		parseAmount(s.Amount).FloatString(2), s.CurrencyCode, oneLine(counterparty(split{Split: s})))
	if splitNo != 0 {
		line += fmt.Sprintf(" (split %d)", splitNo)
	} else if len(targets) > 1 {
		line += fmt.Sprintf(" (all %d splits)", len(targets))
	}
	return line, nil
}

func describeSubscription(s finance.Subscription) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s [subscription_id: %s]: ", oneLine(s.Name), s.Id)
	lo, hi := parseAmount(s.AmountMin), parseAmount(s.AmountMax)
	if lo.Cmp(hi) == 0 {
		b.WriteString(lo.FloatString(2))
	} else {
		b.WriteString(lo.FloatString(2) + " to " + hi.FloatString(2))
	}
	b.WriteString(" " + s.CurrencyCode + " " + s.RepeatFreq)
	if s.Skip > 0 {
		fmt.Fprintf(&b, ", skipping %d between payments", s.Skip)
	}
	if !s.NextExpectedMatch.IsZero() {
		b.WriteString(", next due " + s.NextExpectedMatch.Format(finance.DateFormat))
	}
	if !s.EndDate.IsZero() {
		b.WriteString(", ends " + s.EndDate.Format(finance.DateFormat))
	}
	if !s.Active {
		b.WriteString(" (inactive)")
	}
	return b.String()
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
