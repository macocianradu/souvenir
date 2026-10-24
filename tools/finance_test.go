package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"git.estatecloud.org/radumaco/souvenir/config"
	"git.estatecloud.org/radumaco/souvenir/db/users"
	"git.estatecloud.org/radumaco/souvenir/finance"
)

const fireflyGroups = `[
	{"type":"transactions","id":"10","attributes":{"group_title":null,"transactions":[
		{"transaction_journal_id":"100","type":"withdrawal","date":"2026-10-20T10:00:00+02:00","amount":"42.500000000000","currency_code":"EUR",
		 "description":"Weekly shop","source_id":"1","source_name":"Checking","destination_id":"5","destination_name":"Delhaize",
		 "category_name":"Groceries","budget_name":"Food","tags":["home"],"notes":null}]}},
	{"type":"transactions","id":"11","attributes":{"group_title":"Big order","transactions":[
		{"transaction_journal_id":"101","type":"withdrawal","date":"2026-09-02T10:00:00+02:00","amount":"10.000000000000","currency_code":"EUR",
		 "description":"Cables","source_id":"1","source_name":"Checking","destination_id":"6","destination_name":"Amazon",
		 "category_name":null,"tags":[]},
		{"transaction_journal_id":"102","type":"withdrawal","date":"2026-09-02T10:00:00+02:00","amount":"7.250000000000","currency_code":"EUR",
		 "description":"Bread","source_id":"1","source_name":"Checking","destination_id":"6","destination_name":"Amazon",
		 "category_name":"Groceries","tags":["home","online"]}]}},
	{"type":"transactions","id":"12","attributes":{"group_title":null,"transactions":[
		{"transaction_journal_id":"103","type":"deposit","date":"2026-10-01T09:00:00+02:00","amount":"3000","currency_code":"EUR",
		 "description":"Salary","source_id":"7","source_name":"ACME","destination_id":"1","destination_name":"Checking"}]}}
]`

// fireflyServer answers like Firefly III, serving the groups above over two
// pages, and records each request's method, path, query and body.
func fireflyServer(t *testing.T, seen *[]string) *finance.Client {
	var groups []json.RawMessage
	if err := json.Unmarshal([]byte(fireflyGroups), &groups); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*seen = append(*seen, strings.TrimSpace(r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery+" "+string(body)))
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"message":"Unauthenticated."}`, http.StatusUnauthorized)
			return
		}
		page := func(data ...json.RawMessage) {
			out, _ := json.Marshal(data)
			fmt.Fprintf(w, `{"data":%s,"meta":{"pagination":{"total":3,"total_pages":2}}}`, out)
		}
		switch path := r.URL.Path; {
		case path == "/api/v1/accounts":
			w.Write([]byte(`{"data":[
				{"id":"1","attributes":{"name":"Checking","type":"asset","account_role":"defaultAsset","active":true,"currency_code":"EUR","current_balance":"1200.5","iban":"BE00 1234"}},
				{"id":"2","attributes":{"name":"Savings","type":"asset","account_role":"savingAsset","active":true,"currency_code":"EUR","current_balance":"5000"}}
			],"meta":{"pagination":{"total":2,"total_pages":1}}}`))
		case path == "/api/v1/categories":
			w.Write([]byte(`{"data":[{"id":"3","attributes":{"name":"Groceries"}},{"id":"4","attributes":{"name":"electronics"}}],"meta":{"pagination":{"total_pages":1}}}`))
		case path == "/api/v1/bills" && r.Method == "GET":
			w.Write([]byte(`{"data":[{"id":"8","attributes":{"name":"Netflix","amount_min":"13.99","amount_max":"13.99","currency_code":"EUR",
				"repeat_freq":"monthly","skip":0,"active":true,"next_expected_match":"2026-11-03T00:00:00+01:00"}}],"meta":{"pagination":{"total_pages":1}}}`))
		case path == "/api/v1/bills" && r.Method == "POST":
			var in map[string]any
			json.Unmarshal(body, &in)
			if in["amount_min"] == "13" {
				w.WriteHeader(http.StatusUnprocessableEntity)
				w.Write([]byte(`{"message":"The given data was invalid.","errors":{"currency_code":["The currency code is invalid."]}}`))
				return
			}
			fmt.Fprintf(w, `{"data":{"id":"9","attributes":{"name":%q,"amount_min":%q,"amount_max":%q,"currency_code":"EUR","repeat_freq":%q,"active":true}}}`,
				in["name"], in["amount_min"], in["amount_max"], in["repeat_freq"])
		case path == "/api/v1/transactions" || path == "/api/v1/accounts/1/transactions":
			if r.URL.Query().Get("page") == "2" {
				page(groups[2])
				return
			}
			page(groups[:2]...)
		case strings.HasPrefix(path, "/api/v1/transactions/1") && len(path) == len("/api/v1/transactions/10"):
			fmt.Fprintf(w, `{"data":%s}`, groups[path[len(path)-1]-'0'])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return finance.New(config.FinanceConfig{Url: srv.URL + "/", Token: "secret", Timeout: 1000})
}

func TestFinanceTools_OnlyForTheNamedUsers(t *testing.T) {
	var seen []string
	p := FinanceTools(fireflyServer(t, &seen), []string{"radu"})
	if got := p.Tools(users.WithUser(context.Background(), users.User{Id: "1", Name: "radu"})); len(got) != 8 {
		t.Fatalf("radu gets %d tools", len(got))
	}
	if got := p.Tools(users.WithUser(context.Background(), users.User{Id: "2", Name: "guest"})); got != nil {
		t.Fatal("another user is offered the finance tools")
	}
	if got := p.Tools(context.Background()); got != nil {
		t.Fatal("a request without a user is offered the finance tools")
	}
	for _, tool := range p.Catalog {
		writes := tool.Name == "finance_assign" || tool.Name == "finance_subscription_create"
		if writes != tool.Sensitive {
			t.Fatalf("%s: sensitive is %v", tool.Name, tool.Sensitive)
		}
		if !writes && !tool.Untrusted {
			t.Fatalf("%s returns bank descriptions, so it must be untrusted", tool.Name)
		}
	}
}

func TestFinanceTransactions_ReadsEveryPageAndFilters(t *testing.T) {
	var seen []string
	tools := financeTools(fireflyServer(t, &seen))
	out := runTool(t, tools, "finance_transactions", `{"start":"2026-09-01","end":"2026-10-31","account_id":1}`)
	for _, want := range []string{
		"4 matching transactions from 2026-09-01 to 2026-10-31",
		"withdrawals -59.75 EUR", "deposits +3000.00 EUR",
		"2026-10-20 -42.50 EUR, Checking → Delhaize: Weekly shop (category: Groceries; budget: Food; tags: home) [transaction_id: 10]",
		"[transaction_id: 11, split 1 of 2]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "Weekly shop") > strings.Index(out, "Salary") || strings.Index(out, "Salary") > strings.Index(out, "Cables") {
		t.Fatalf("not newest first:\n%s", out)
	}
	if !strings.Contains(seen[0], "GET /api/v1/accounts/1/transactions?") || !strings.Contains(seen[0], "start=2026-09-01") ||
		!strings.Contains(seen[0], "end=2026-10-31") || !strings.Contains(seen[1], "page=2") {
		t.Fatal(seen)
	}

	out = runTool(t, tools, "finance_transactions", `{"category":"groceries","payee":"amaz","limit":1}`)
	if !strings.Contains(out, "1 matching") || !strings.Contains(out, "Bread") || strings.Contains(out, "Weekly") {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_transactions", `{"category":"none","type":"withdrawal"}`); !strings.Contains(out, "1 matching") || !strings.Contains(out, "Cables") {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_transactions", `{"min_amount":20,"max_amount":100}`); !strings.Contains(out, "1 matching") || !strings.Contains(out, "Weekly shop") {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_transactions", `{"search":"big order"}`); !strings.Contains(out, "2 matching") {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_transactions", `{"start":"October"}`); !strings.HasPrefix(out, "Error: start must be a date") {
		t.Fatal(out)
	}
	for _, s := range seen {
		if !strings.HasPrefix(s, "GET ") {
			t.Fatalf("sent %s", s)
		}
	}
}

func TestFinanceSummary_GroupsSpending(t *testing.T) {
	var seen []string
	tools := financeTools(fireflyServer(t, &seen))
	out := runTool(t, tools, "finance_summary", `{"group_by":"category"}`)
	for _, want := range []string{"Withdrawals from", "3 transactions", "EUR, total 59.75:",
		"- Groceries: 49.75 (83.3%, 2 transactions)", "- (none): 10.00 (16.7%, 1 transactions)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(seen[0], "type=withdrawal") {
		t.Fatal(seen[0])
	}
	out = runTool(t, tools, "finance_summary", `{"group_by":"month"}`)
	if !strings.Contains(out, "- 2026-09: 17.25") || strings.Index(out, "2026-09") > strings.Index(out, "2026-10") {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_summary", `{"group_by":"payee","type":"deposit"}`); !strings.Contains(out, "- ACME: 3000.00 (100.0%") {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_summary", `{"group_by":"tag"}`); !strings.Contains(out, "- home: 49.75") || !strings.Contains(out, "- online: 7.25") {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_summary", `{"group_by":"weekday"}`); !strings.HasPrefix(out, "Error: group_by must be") {
		t.Fatal(out)
	}
}

func TestFinanceAccountsAndTransaction(t *testing.T) {
	var seen []string
	tools := financeTools(fireflyServer(t, &seen))
	out := runTool(t, tools, "finance_accounts", `{"date":"2026-10-01"}`)
	for _, want := range []string{"Checking [account_id: 1]: 1200.50 EUR (defaultAsset, BE00 1234)", "Savings [account_id: 2]: 5000.00 EUR",
		"Total of the active accounts above: 6200.50 EUR"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(seen[0], "type=asset") || !strings.Contains(seen[0], "date=2026-10-01") {
		t.Fatal(seen[0])
	}
	out = runTool(t, tools, "finance_transaction", `{"transaction_id":11}`)
	for _, want := range []string{"Transaction 11: Big order", "Split 1 of 2:", "Description: Cables", "Amount: -10.00 EUR",
		"To: Amazon [account_id: 6]", "Split 2 of 2:", "Tags: home, online"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if out := runTool(t, tools, "finance_transaction", `{"transaction_id":99}`); out != "Error: not found" {
		t.Fatal(out)
	}
}

func TestFinanceAssign_SetsOnlyTheCategoryOnTheChosenSplit(t *testing.T) {
	var seen []string
	tools := financeTools(fireflyServer(t, &seen))
	out := runTool(t, tools, "finance_assign", `{"transactions":[{"transaction_id":11,"split":1},{"transaction_id":10}],"category":"ELECTRONICS"}`)
	for _, want := range []string{"Set category electronics on 2 of 2 transactions", "transaction 11, 2026-09-02 -10.00 EUR to Amazon (split 1)",
		"transaction 10, 2026-10-20 -42.50 EUR to Delhaize"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	var puts []string
	for _, s := range seen {
		if strings.HasPrefix(s, "PUT ") {
			puts = append(puts, s)
		} else if !strings.HasPrefix(s, "GET ") {
			t.Fatalf("sent %s", s)
		}
	}
	want := []string{
		`PUT /api/v1/transactions/11? {"apply_rules":false,"transactions":[{"transaction_journal_id":"101","category_id":"4"},{"transaction_journal_id":"102"}]}`,
		`PUT /api/v1/transactions/10? {"apply_rules":false,"transactions":[{"transaction_journal_id":"100","category_id":"4"}]}`,
	}
	if strings.Join(puts, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent\n%s", strings.Join(puts, "\n"))
	}

	seen = nil
	out = runTool(t, tools, "finance_assign", `{"transactions":[{"transaction_id":12},{"transaction_id":11,"split":3},{"transaction_id":99}],"subscription":"netflix"}`)
	if !strings.Contains(out, "Set subscription Netflix on 1 of 3") || !strings.Contains(out, "transaction 11: not changed, it has 2 splits, not a split 3") ||
		!strings.Contains(out, "transaction 99: not changed, not found") || !strings.Contains(seen[len(seen)-3], `"bill_id":"8"`) {
		t.Fatalf("%s\n%v", out, seen)
	}
	if out := runTool(t, tools, "finance_assign", `{"transactions":[{"transaction_id":10}],"category":"Fun"}`); !strings.Contains(out, `no category "Fun"`) {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_assign", `{"transactions":[{"transaction_id":10}]}`); !strings.HasPrefix(out, "Error: give a category") {
		t.Fatal(out)
	}
}

func TestFinanceSubscriptionCreate(t *testing.T) {
	var seen []string
	tools := financeTools(fireflyServer(t, &seen))
	out := runTool(t, tools, "finance_subscription_create", `{"name":"Spotify","amount_min":10.99,"date":"2026-11-05","repeat_freq":"monthly"}`)
	if out != "Created Spotify [subscription_id: 9]: 10.99 EUR monthly" {
		t.Fatal(out)
	}
	if last := seen[len(seen)-1]; last != `POST /api/v1/bills? {"name":"Spotify","amount_min":"10.99","amount_max":"10.99","date":"2026-11-05","repeat_freq":"monthly","skip":0,"active":true}` {
		t.Fatal(last)
	}
	for args, want := range map[string]string{
		`{"name":"netflix","amount_min":13.99,"date":"2026-11-03","repeat_freq":"monthly"}`:                `Error: a subscription named "netflix" already exists: Netflix [subscription_id: 8]: 13.99 EUR monthly, next due 2026-11-03`,
		`{"name":"Gym","amount_min":30,"amount_max":20,"date":"2026-11-01","repeat_freq":"monthly"}`:       "Error: amount_min must be positive and amount_max at least amount_min",
		`{"name":"Gym","amount_min":30,"date":"2026-11-01","repeat_freq":"daily"}`:                         `Error: repeat_freq must be weekly, monthly, quarterly, half-year or yearly, not "daily"`,
		`{"name":"Gym","amount_min":30,"repeat_freq":"monthly"}`:                                           "Error: date is required",
		`{"name":"Gym","amount_min":13,"currency_code":"XXX","date":"2026-11-01","repeat_freq":"monthly"}`: "Error: firefly iii refused the request: currency_code: The currency code is invalid.",
	} {
		if got := runTool(t, tools, "finance_subscription_create", args); got != want {
			t.Fatalf("%s:\n got %s\nwant %s", args, got, want)
		}
	}
	if out := runTool(t, tools, "finance_subscriptions", `{}`); out != "Netflix [subscription_id: 8]: 13.99 EUR monthly, next due 2026-11-03" {
		t.Fatal(out)
	}
	if out := runTool(t, tools, "finance_categories", `{}`); out != "electronics\nGroceries" {
		t.Fatal(out)
	}
}
