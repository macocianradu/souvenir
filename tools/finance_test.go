package tools

import (
	"context"
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
// pages, and records each request's method, path and query.
func fireflyServer(t *testing.T, seen *[]string) *finance.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"message":"Unauthenticated."}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/accounts":
			w.Write([]byte(`{"data":[
				{"id":"1","attributes":{"name":"Checking","type":"asset","account_role":"defaultAsset","active":true,"currency_code":"EUR","current_balance":"1200.5","iban":"BE00 1234"}},
				{"id":"2","attributes":{"name":"Savings","type":"asset","account_role":"savingAsset","active":true,"currency_code":"EUR","current_balance":"5000"}}
			],"meta":{"pagination":{"total":2,"total_pages":1}}}`))
		case "/api/v1/transactions", "/api/v1/accounts/1/transactions":
			groups := strings.SplitN(fireflyGroups, "\n\t{\"type\"", 4)
			if r.URL.Query().Get("page") == "2" {
				w.Write([]byte(`{"data":[{"type"` + groups[3] + `,"meta":{"pagination":{"total":3,"total_pages":2}}}`))
				return
			}
			first := strings.TrimSuffix(strings.Join(groups[:3], "\n\t{\"type\""), ",")
			w.Write([]byte(`{"data":` + first + `],"meta":{"pagination":{"total":3,"total_pages":2}}}`))
		case "/api/v1/transactions/11":
			groups := strings.SplitN(fireflyGroups, "\n\t{\"type\"", 4)
			w.Write([]byte(`{"data":{"type"` + strings.TrimSuffix(strings.TrimSpace(groups[2]), ",") + `}`))
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
	if got := p.Tools(users.WithUser(context.Background(), users.User{Id: "1", Name: "radu"})); len(got) != 4 {
		t.Fatalf("radu gets %d tools", len(got))
	}
	if got := p.Tools(users.WithUser(context.Background(), users.User{Id: "2", Name: "guest"})); got != nil {
		t.Fatal("another user is offered the finance tools")
	}
	if got := p.Tools(context.Background()); got != nil {
		t.Fatal("a request without a user is offered the finance tools")
	}
	for _, tool := range p.Catalog {
		if !tool.Untrusted {
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
		"(one of 2 splits) [transaction_id: 11]",
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
