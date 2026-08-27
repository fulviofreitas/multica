package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	testutil "github.com/multica-ai/multica/server/internal/testutil"
)

func newContactSalesRequest(body CreateContactSalesRequest) *http.Request {
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req := httptest.NewRequest("POST", "/api/contact-sales", &buf)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func validContactSalesRequest() CreateContactSalesRequest {
	return CreateContactSalesRequest{
		FirstName:       "Ada",
		LastName:        "Lovelace",
		BusinessEmail:   "ada@analytical-engine.example",
		CompanyName:     "Analytical Engine Co.",
		CompanySize:     "11-50",
		CountryRegion:   "United Kingdom",
		UseCase:         "evaluate",
		Goals:           "We want to compound agent productivity across the team.",
		ConsentOutreach: true,
		ConsentUpdates:  false,
	}
}

func clearContactSalesForEmail(t *testing.T, email string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`DELETE FROM contact_sales_inquiry WHERE business_email = $1`, email); err != nil {
		t.Fatalf("clear contact_sales_inquiry: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(),
			`DELETE FROM contact_sales_inquiry WHERE business_email = $1`, email)
	})
}

func TestCreateContactSalesHappyPath(t *testing.T) {
	body := validContactSalesRequest()
	clearContactSalesForEmail(t, body.BusinessEmail)

	w := testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusCreated)
	var resp ContactSalesResponse
	w.Decode(&resp)
	if resp.ID == "" {
		t.Fatal("expected inquiry id in response")
	}
}

func TestCreateContactSalesRejectsFreeEmail(t *testing.T) {
	body := validContactSalesRequest()
	body.BusinessEmail = "ada@gmail.com"

	testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusBadRequest)
}

func TestCreateContactSalesRejectsInvalidEmail(t *testing.T) {
	body := validContactSalesRequest()
	body.BusinessEmail = "not-an-email"

	testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusBadRequest)
}

func TestCreateContactSalesRejectsUnknownCompanySize(t *testing.T) {
	body := validContactSalesRequest()
	body.CompanySize = "ten-ish"

	testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusBadRequest)
}

func TestCreateContactSalesRejectsUnknownUseCase(t *testing.T) {
	body := validContactSalesRequest()
	body.UseCase = "world-domination"

	testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusBadRequest)
}

func TestCreateContactSalesMissingFirstName(t *testing.T) {
	body := validContactSalesRequest()
	body.FirstName = "   "

	testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusBadRequest)
}

func TestCreateContactSalesPerEmailRateLimit(t *testing.T) {
	body := validContactSalesRequest()
	body.BusinessEmail = "ratelimit-ada@analytical-engine.example"
	clearContactSalesForEmail(t, body.BusinessEmail)

	for i := 0; i < contactSalesHourlyEmailCap; i++ {
		w := testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body))
		if w.Code != http.StatusCreated {
			t.Fatalf("iteration %d: expected 201, got %d: %s", i, w.Code, w.Body.String())
		}
	}

	testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusTooManyRequests)
}

func TestIsBusinessEmailDomain(t *testing.T) {
	cases := []struct {
		email string
		want  bool
	}{
		{"ada@multica.ai", true},
		{"ada@example.com", true},
		{"ada@gmail.com", false},
		{"ada@Gmail.COM", false},
		{"ada@yahoo.co.uk", false},
		{"ada@qq.com", false},
		{"weird-no-at", false},
		{"ada@", false},
	}
	for _, c := range cases {
		got := isBusinessEmailDomain(c.email)
		if got != c.want {
			t.Errorf("isBusinessEmailDomain(%q) = %v, want %v", c.email, got, c.want)
		}
	}
}

func TestCanonicalBusinessEmail(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
		wantOk bool
	}{
		{"plain", "ada@multica.ai", "ada@multica.ai", true},
		{"uppercase normalized", "Ada@Multica.AI", "ada@multica.ai", true},
		{"trim whitespace", "  ada@multica.ai  ", "ada@multica.ai", true},
		{"display name stripped", "Ada Lovelace <ada@multica.ai>", "ada@multica.ai", true},
		{"angle-bracketed", "<ada@multica.ai>", "ada@multica.ai", true},
		{"empty", "", "", false},
		{"only whitespace", "   ", "", false},
		{"missing at", "no-at-sign", "", false},
		{"missing local", "@multica.ai", "", false},
		{"missing domain", "ada@", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := canonicalBusinessEmail(c.input)
			if ok != c.wantOk || got != c.want {
				t.Errorf("canonicalBusinessEmail(%q) = (%q, %v), want (%q, %v)",
					c.input, got, ok, c.want, c.wantOk)
			}
		})
	}
}

// Regression: net/mail.ParseAddress accepts display-name forms like
// `Ada <ada@gmail.com>`. Earlier versions of the handler ran the
// free-email check against the raw user input, so the domain it saw was
// `gmail.com>` (with the trailing angle bracket) — which is not in the
// block list, allowing a personal address to bypass the gate. The handler
// must canonicalize through the parsed addr.Address before the check.
func TestCreateContactSalesRejectsFreeEmailWithDisplayName(t *testing.T) {
	body := validContactSalesRequest()
	body.BusinessEmail = "Ada Lovelace <ada@gmail.com>"

	testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusBadRequest)
}

func TestCreateContactSalesNormalizesDisplayNameEmail(t *testing.T) {
	body := validContactSalesRequest()
	body.BusinessEmail = "Ada Lovelace <ada@display-name.example>"
	clearContactSalesForEmail(t, "ada@display-name.example")

	testutil.Call(t, testHandler.CreateContactSales, newContactSalesRequest(body)).Want(http.StatusCreated)

	var stored string
	if err := testPool.QueryRow(context.Background(),
		`SELECT business_email FROM contact_sales_inquiry WHERE business_email = $1`,
		"ada@display-name.example").Scan(&stored); err != nil {
		t.Fatalf("expected row with canonical email persisted: %v", err)
	}
	if stored != "ada@display-name.example" {
		t.Fatalf("expected canonical email persisted, got %q", stored)
	}
}
