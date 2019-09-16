// Copyright 2018 The Moov Authors
// Use of this source code is governed by an Apache License
// license that can be found in the LICENSE file.

package paygate

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	accounts "github.com/moov-io/accounts/client"
	"github.com/moov-io/ach"
	"github.com/moov-io/base"
	"github.com/moov-io/base/admin"
	"github.com/moov-io/paygate/internal/database"
	"github.com/moov-io/paygate/pkg/achclient"

	"github.com/go-kit/kit/log"
	"github.com/gorilla/mux"
)

func makeTestODFIAccount() *ODFIAccount {
	return &ODFIAccount{
		routingNumber: "121042882", // set as ODFIIdentification in PPD batches (used in tests)
		accountID:     "odfi-account",
	}
}

func TestODFIAccount(t *testing.T) {
	accountsClient := &testAccountsClient{}
	odfi := &ODFIAccount{
		client:        accountsClient,
		accountNumber: "",
		routingNumber: "",
		accountType:   Savings,
		accountID:     "accountID",
	}

	orig, dep := odfi.metadata()
	if orig.ID != "odfi" {
		t.Errorf("originator: %#v", orig)
	}
	if string(dep.ID) != "odfi" {
		t.Errorf("depository: %#v", dep)
	}

	if accountID, err := odfi.getID("", "userID"); accountID != "accountID" || err != nil {
		t.Errorf("accountID=%s error=%v", accountID, err)
	}
	odfi.accountID = "" // unset so we make the AccountsClient call
	accountsClient.accounts = []accounts.Account{
		{
			ID: "accountID2",
		},
	}
	if accountID, err := odfi.getID("", "userID"); accountID != "accountID2" || err != nil {
		t.Errorf("accountID=%s error=%v", accountID, err)
	}
	if odfi.accountID != "accountID2" {
		t.Errorf("odfi.accountID=%s", odfi.accountID)
	}

	// error on AccountsClient call
	odfi.accountID = ""
	accountsClient.err = errors.New("bad")
	if accountID, err := odfi.getID("", "userID"); accountID != "" || err == nil {
		t.Errorf("expected error accountID=%s", accountID)
	}

	// on nil AccountsClient expect an error
	odfi.client = nil
	if accountID, err := odfi.getID("", "userID"); accountID != "" || err == nil {
		t.Errorf("expcted error accountID=%s", accountID)
	}
}

func TestMicroDeposits__json(t *testing.T) {
	amt, _ := NewAmount("USD", "1.24")
	bs, err := json.Marshal([]microDeposit{
		{amount: *amt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v := string(bs); v != `[{"amount":"USD 1.24"}]` {
		t.Error(v)
	}
}

func TestMicroDeposits__AdminGetMicroDeposits(t *testing.T) {
	svc := admin.NewServer(":0")
	go func(t *testing.T) {
		if err := svc.Listen(); err != nil && err != http.ErrServerClosed {
			t.Fatal(err)
		}
	}(t)
	defer svc.Shutdown()

	amt1, _ := NewAmount("USD", "0.11")
	amt2, _ := NewAmount("USD", "0.32")
	depRepo := &mockDepositoryRepository{
		microDeposits: []*microDeposit{
			{amount: *amt1},
			{amount: *amt2},
		},
	}
	AddMicroDepositAdminRoutes(log.NewNopLogger(), svc, depRepo)

	req, err := http.NewRequest("GET", "http://localhost"+svc.BindAddr()+"/depositories/foo/micro-deposits", nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("bogus HTTP status: %s", resp.Status)
	}

	defer resp.Body.Close()

	bs, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Error(err)
	}
	t.Log(string(bytes.TrimSpace(bs)))

	type response struct {
		Amount Amount `json:"amount"`
	}
	var rs []response
	if err := json.NewDecoder(bytes.NewReader(bs)).Decode(&rs); err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Errorf("got %d micro-deposits", len(rs))
	}
	for i := range rs {
		switch v := rs[i].Amount.String(); v {
		case "USD 0.11", "USD 0.32":
			t.Logf("matched %s", v)
		default:
			t.Errorf("got %s", v)
		}
	}

	// bad case, DepositoryRepository returns an error
	depRepo.err = errors.New("bad error")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bogus HTTP status: %s", resp.Status)
	}
}

func TestMicroDeposits__microDepositAmounts(t *testing.T) {
	for i := 0; i < 100; i++ {
		amounts := microDepositAmounts()
		if len(amounts) != 2 {
			t.Errorf("got %d micro-deposit amounts", len(amounts))
		}
		sum, _ := amounts[0].Plus(amounts[1])
		if sum.Int() != (amounts[0].Int() + amounts[1].Int()) {
			t.Errorf("amounts[0]=%s amounts[1]=%s", amounts[0].String(), amounts[1].String())
		}
	}
}

func TestMicroDeposits__confirmMicroDeposits(t *testing.T) {
	type state struct {
		guesses       []Amount
		microDeposits []*microDeposit
	}
	testCases := []struct {
		name               string
		state              state
		expectedErrMessage string
	}{
		{
			"There are 0 microdeposits",
			state{
				microDeposits: []*microDeposit{},
				guesses:       []Amount{},
			},
			"unable to confirm micro deposits, got 0 micro deposits",
		},
		{
			"There are less guesses than microdeposits",
			state{
				microDeposits: []*microDeposit{
					{amount: Amount{number: 10, symbol: "USD"}},
					{amount: Amount{number: 4, symbol: "USD"}},
				},
				guesses: []Amount{
					{number: 10, symbol: "USD"},
				},
			},
			"incorrect amount of guesses, got 1",
		},
		{
			"There are more guesses than microdeposits",
			state{
				microDeposits: []*microDeposit{
					{amount: Amount{number: 10, symbol: "USD"}},
					{amount: Amount{number: 4, symbol: "USD"}},
				},
				guesses: []Amount{
					{number: 10, symbol: "USD"},
					{number: 4, symbol: "USD"},
					{number: 7, symbol: "USD"},
				},
			},
			"incorrect amount of guesses, got 3",
		},
		{
			"One guess is correct, the other is wrong",
			state{
				microDeposits: []*microDeposit{
					{amount: Amount{number: 10, symbol: "USD"}},
					{amount: Amount{number: 4, symbol: "USD"}},
				},
				guesses: []Amount{
					{number: 10, symbol: "USD"},
					{number: 7, symbol: "USD"},
				},
			},
			"incorrect micro deposit guesses",
		},
		{
			"Both guesses are wrong",
			state{
				microDeposits: []*microDeposit{
					{amount: Amount{number: 10, symbol: "USD"}},
					{amount: Amount{number: 4, symbol: "USD"}},
				},
				guesses: []Amount{
					{number: 1, symbol: "USD"},
					{number: 7, symbol: "USD"},
				},
			},
			"incorrect micro deposit guesses",
		},
		{
			"Both guesses are correct",
			state{
				microDeposits: []*microDeposit{
					{amount: Amount{number: 10, symbol: "USD"}},
					{amount: Amount{number: 4, symbol: "USD"}},
				},
				guesses: []Amount{
					{number: 10, symbol: "USD"},
					{number: 4, symbol: "USD"},
				},
			},
			"",
		},
		{
			"Both guesses are correct, in the opposite order",
			state{
				microDeposits: []*microDeposit{
					{amount: Amount{number: 10, symbol: "USD"}},
					{amount: Amount{number: 4, symbol: "USD"}},
				},
				guesses: []Amount{
					{number: 4, symbol: "USD"},
					{number: 10, symbol: "USD"},
				},
			},
			"",
		},
	}

	sqlite := database.CreateTestSqliteDB(t)
	defer sqlite.Close()
	mysql := database.CreateTestMySQLDB(t)
	defer mysql.Close()
	databases := []*SQLDepositoryRepo{
		{sqlite.DB, log.NewNopLogger()},
		{mysql.DB, log.NewNopLogger()},
	}

	for _, db := range databases {
		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				depositoryID := DepositoryID(base.ID())
				userID := base.ID()

				if err := db.initiateMicroDeposits(depositoryID, userID, tc.state.microDeposits); err != nil {
					t.Fatal(err)
				}

				err := db.confirmMicroDeposits(depositoryID, userID, tc.state.guesses)
				if tc.expectedErrMessage == "" {
					if err != nil {
						t.Errorf("nil was the expected result, got '%s' instead", err)
					}
				} else {
					if err == nil {
						t.Error("expected an error message, got nil instead")
					}
					if err.Error() != tc.expectedErrMessage {
						t.Errorf("'%s' was the expected error, got '%s' instead", tc.expectedErrMessage, err.Error())
					}
				}
			})
		}
	}
}

func TestMicroDeposits__insertMicroDepositVerify(t *testing.T) {
	t.Parallel()

	check := func(t *testing.T, repo DepositoryRepository) {
		id, userID := DepositoryID(base.ID()), base.ID()

		amt, _ := NewAmount("USD", "0.11")
		mc := &microDeposit{amount: *amt, fileID: base.ID() + "-micro-deposit-verify"}
		mcs := []*microDeposit{mc}

		if err := repo.initiateMicroDeposits(id, userID, mcs); err != nil {
			t.Fatal(err)
		}

		// TODO(adam): verify transactionID is stored and returned

		microDeposits, err := repo.getMicroDepositsForUser(id, userID)
		if n := len(microDeposits); err != nil || n == 0 {
			t.Fatalf("n=%d error=%v", n, err)
		}
		if m := microDeposits[0]; m.fileID != mc.fileID {
			t.Errorf("got %s", m.fileID)
		}
	}

	// SQLite tests
	sqliteDB := database.CreateTestSqliteDB(t)
	defer sqliteDB.Close()
	check(t, &SQLDepositoryRepo{sqliteDB.DB, log.NewNopLogger()})

	// MySQL tests
	mysqlDB := database.CreateTestMySQLDB(t)
	defer mysqlDB.Close()
	check(t, &SQLDepositoryRepo{mysqlDB.DB, log.NewNopLogger()})
}

func TestMicroDeposits__initiateError(t *testing.T) {
	id, userID := DepositoryID(base.ID()), base.ID()
	depRepo := &mockDepositoryRepository{err: errors.New("bad error")}
	router := &DepositoryRouter{
		logger:         log.NewNopLogger(),
		depositoryRepo: depRepo,
	}
	r := mux.NewRouter()
	router.RegisterRoutes(r, true) // disable Accounts service calls

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/depositories/%s/micro-deposits", id), nil)
	req.Header.Set("x-user-id", userID)
	r.ServeHTTP(w, req)
	w.Flush()

	if w.Code != http.StatusBadRequest {
		t.Errorf("bogus HTTP status %d: %s", w.Code, w.Body.String())
	}
}

func TestMicroDeposits__confirmError(t *testing.T) {
	id, userID := DepositoryID(base.ID()), base.ID()
	depRepo := &mockDepositoryRepository{err: errors.New("bad error")}
	router := &DepositoryRouter{
		logger:         log.NewNopLogger(),
		depositoryRepo: depRepo,
	}
	r := mux.NewRouter()
	router.RegisterRoutes(r, true) // disable Accounts service calls

	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(confirmDepositoryRequest{
		Amounts: []string{"USD 0.11"}, // doesn't matter as we error anyway
	})
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/depositories/%s/micro-deposits/confirm", id), &buf)
	req.Header.Set("x-user-id", userID)
	r.ServeHTTP(w, req)
	w.Flush()

	if w.Code != http.StatusBadRequest {
		t.Errorf("bogus HTTP status %d: %s", w.Code, w.Body.String())
	}
}

func TestMicroDeposits__routes(t *testing.T) {
	t.Parallel()

	check := func(t *testing.T, db *sql.DB) {
		id, userID := DepositoryID(base.ID()), base.ID()

		depRepo := &SQLDepositoryRepo{db, log.NewNopLogger()}
		eventRepo := &SQLEventRepo{db, log.NewNopLogger()}

		// Write depository
		dep := &Depository{
			ID:            id,
			BankName:      "bank name",
			Holder:        "holder",
			HolderType:    Individual,
			Type:          Checking,
			RoutingNumber: "121042882",
			AccountNumber: "151",
			Status:        DepositoryUnverified, // status is checked in initiateMicroDeposits
			Created:       base.NewTime(time.Now().Add(-1 * time.Second)),
		}
		if err := depRepo.upsertUserDepository(userID, dep); err != nil {
			t.Fatal(err)
		}

		accountID := base.ID()
		accountsClient := &testAccountsClient{
			accounts: []accounts.Account{{ID: accountID}},
			transaction: &accounts.Transaction{
				ID: base.ID(),
			},
		}
		fedClient, ofacClient := &testFEDClient{}, &testOFACClient{}

		achClient, _, server := achclient.MockClientServer("micro-deposits", func(r *mux.Router) {
			achclient.AddCreateRoute(nil, r)
			achclient.AddValidateRoute(r)
		})
		defer server.Close()

		testODFIAccount := makeTestODFIAccount()

		router := &DepositoryRouter{
			logger:         log.NewNopLogger(),
			odfiAccount:    testODFIAccount,
			accountsClient: accountsClient,
			achClient:      achClient,
			fedClient:      fedClient,
			ofacClient:     ofacClient,
			depositoryRepo: depRepo,
			eventRepo:      eventRepo,
		}
		r := mux.NewRouter()
		router.RegisterRoutes(r, false)

		// Set ACH_ENDPOINT to override the achclient.New call
		os.Setenv("ACH_ENDPOINT", server.URL)

		// inititate our micro deposits
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", fmt.Sprintf("/depositories/%s/micro-deposits", id), nil)
		req.Header.Set("x-user-id", userID)
		r.ServeHTTP(w, req)
		w.Flush()

		if w.Code != http.StatusCreated {
			if !strings.Contains(w.Body.String(), ":8080: connect: connection refused") {
				t.Errorf("initiate got %d status: %v", w.Code, w.Body.String()) // TODO(adam): Accountslient needs a stub
			}
		}

		// confirm our deposits
		var buf bytes.Buffer
		var request confirmDepositoryRequest
		for i := range accountsClient.postedTransactions {
			for j := range accountsClient.postedTransactions[i].Lines {
				// Only take the credit amounts (as we only need the amount from one side of the dual entry)
				line := accountsClient.postedTransactions[i].Lines[j]

				if line.AccountID == accountID && strings.EqualFold(line.Purpose, "ACHCredit") {
					request.Amounts = append(request.Amounts, fmt.Sprintf("USD 0.%02d", line.Amount))
				}
			}
		}
		if len(request.Amounts) != 2 {
			t.Errorf("got %d amounts", len(request.Amounts))
		}
		if err := json.NewEncoder(&buf).Encode(request); err != nil {
			t.Fatal(err)
		}

		w = httptest.NewRecorder()
		req = httptest.NewRequest("POST", fmt.Sprintf("/depositories/%s/micro-deposits/confirm", id), &buf)
		req.Header.Set("x-user-id", userID)
		r.ServeHTTP(w, req)
		w.Flush()

		if w.Code != http.StatusOK {
			t.Errorf("confirm got %d status: %v", w.Code, w.Body.String())
		}
	}

	// SQLite tests
	sqliteDB := database.CreateTestSqliteDB(t)
	defer sqliteDB.Close()
	check(t, sqliteDB.DB)

	// MySQL tests
	mysqlDB := database.CreateTestMySQLDB(t)
	defer mysqlDB.Close()
	check(t, mysqlDB.DB)
}

func TestMicroDeposits__markMicroDepositAsMerged(t *testing.T) {
	t.Parallel()

	check := func(t *testing.T, repo *SQLDepositoryRepo) {
		amt, _ := NewAmount("USD", "0.11")
		microDeposits := []*microDeposit{
			{amount: *amt, fileID: "fileID"},
		}
		if err := repo.initiateMicroDeposits(DepositoryID("id"), "userID", microDeposits); err != nil {
			t.Fatal(err)
		}

		mc := uploadableMicroDeposit{
			depositoryID: "id",
			userID:       "userID",
			amount:       amt,
			fileID:       "fileID",
		}
		if err := repo.markMicroDepositAsMerged("filename", mc); err != nil {
			t.Fatal(err)
		}

		// Read merged_filename and verify
		mergedFilename, err := readMergedFilename(repo, amt, DepositoryID(mc.depositoryID))
		if err != nil {
			t.Fatal(err)
		}
		if mergedFilename != "filename" {
			t.Errorf("mergedFilename=%s", mergedFilename)
		}
	}

	// SQLite tests
	sqliteDB := database.CreateTestSqliteDB(t)
	defer sqliteDB.Close()
	check(t, &SQLDepositoryRepo{sqliteDB.DB, log.NewNopLogger()})

	// MySQL tests
	mysqlDB := database.CreateTestMySQLDB(t)
	defer mysqlDB.Close()
	check(t, &SQLDepositoryRepo{mysqlDB.DB, log.NewNopLogger()})
}

func TestMicroDepositCursor__next(t *testing.T) {
	sqliteDB := database.CreateTestSqliteDB(t)
	defer sqliteDB.Close()

	depRepo := &SQLDepositoryRepo{sqliteDB.DB, log.NewNopLogger()}
	cur := depRepo.getMicroDepositCursor(2)

	microDeposits, err := cur.Next()
	if len(microDeposits) != 0 || err != nil {
		t.Fatalf("microDeposits=%#v error=%v", microDeposits, err)
	}

	// Write a micro-deposit
	amt, _ := NewAmount("USD", "0.11")
	if err := depRepo.initiateMicroDeposits(DepositoryID("id"), "userID", []*microDeposit{{amount: *amt, fileID: "fileID"}}); err != nil {
		t.Fatal(err)
	}
	// our cursor should return this micro-deposit now since there's no mergedFilename
	microDeposits, err = cur.Next()
	if len(microDeposits) != 1 || err != nil {
		t.Fatalf("microDeposits=%#v error=%v", microDeposits, err)
	}
	if microDeposits[0].depositoryID != "id" || microDeposits[0].amount.String() != "USD 0.11" {
		t.Errorf("microDeposits[0]=%#v", microDeposits[0])
	}
	mc := microDeposits[0] // save for later

	// verify calling our cursor again returns nothing
	microDeposits, err = cur.Next()
	if len(microDeposits) != 0 || err != nil {
		t.Fatalf("microDeposits=%#v error=%v", microDeposits, err)
	}

	// mark the micro-deposit as merged (via merged_filename) and re-create the cursor to expect nothing returned in Next()
	cur = depRepo.getMicroDepositCursor(2)
	if err := depRepo.markMicroDepositAsMerged("filename", mc); err != nil {
		t.Fatal(err)
	}
	microDeposits, err = cur.Next()
	if len(microDeposits) != 0 || err != nil {
		t.Fatalf("microDeposits=%#v error=%v", microDeposits, err)
	}

	// verify merged_filename
	filename, err := readMergedFilename(depRepo, mc.amount, DepositoryID(mc.depositoryID))
	if err != nil {
		t.Fatal(err)
	}
	if filename != "filename" {
		t.Errorf("mc=%#v", mc)
	}
}

func readMergedFilename(repo *SQLDepositoryRepo, amount *Amount, id DepositoryID) (string, error) {
	query := `select merged_filename from micro_deposits where amount = ? and depository_id = ? limit 1;`
	stmt, err := repo.db.Prepare(query)
	if err != nil {
		return "", err
	}
	defer stmt.Close()

	var mergedFilename string
	if err := stmt.QueryRow(amount.String(), id).Scan(&mergedFilename); err != nil {
		return "", err
	}
	return mergedFilename, nil
}

func TestMicroDeposits__addMicroDeposit(t *testing.T) {
	amt, _ := NewAmount("USD", "0.28")

	ed := ach.NewEntryDetail()
	ed.TransactionCode = ach.CheckingCredit
	ed.TraceNumber = "123"
	ed.Amount = 12 // $0.12

	bh := ach.NewBatchHeader()
	bh.StandardEntryClassCode = "PPD"
	batch, err := ach.NewBatch(bh)
	if err != nil {
		t.Fatal(err)
	}
	batch.AddEntry(ed)

	file := ach.NewFile()
	file.AddBatch(batch)

	if err := addMicroDeposit(file, *amt); err != nil {
		t.Fatal(err)
	}
	if len(file.Batches) != 1 || len(file.Batches[0].GetEntries()) != 2 {
		t.Fatalf("file.Batches[0]=%#v", file.Batches[0])
	}

	ed = file.Batches[0].GetEntries()[1]
	if ed.Amount != amt.Int() {
		t.Errorf("got ed.Amount=%d", ed.Amount)
	}

	// bad path
	if err := addMicroDeposit(nil, *amt); err == nil {
		t.Error("expected error")
	}
}

func TestMicroDeposits__addMicroDepositWithdraw(t *testing.T) {
	ed := ach.NewEntryDetail()
	ed.TransactionCode = ach.CheckingCredit
	ed.TraceNumber = "123"
	ed.Amount = 12 // $0.12

	bh := ach.NewBatchHeader()
	bh.StandardEntryClassCode = "PPD"
	batch, err := ach.NewBatch(bh)
	if err != nil {
		t.Fatal(err)
	}
	batch.AddEntry(ed)

	file := ach.NewFile()
	file.AddBatch(batch)

	withdrawAmount, _ := NewAmount("USD", "0.14") // not $0.12 on purpose

	// nil, so expect no changes
	if err := addMicroDepositWithdraw(nil, withdrawAmount); err == nil {
		t.Fatal("expected error")
	}
	if len(file.Batches) != 1 || len(file.Batches[0].GetEntries()) != 1 {
		t.Fatalf("file.Batches[0]=%#v", file.Batches[0])
	}

	// add reversal batch
	if err := addMicroDepositWithdraw(file, withdrawAmount); err != nil {
		t.Fatal(err)
	}

	// verify
	if len(file.Batches) != 1 {
		t.Fatalf("file.Batches=%#v", file.Batches)
	}
	entries := file.Batches[0].GetEntries()
	if len(entries) != 2 {
		t.Fatalf("entries=%#v", entries)
	}
	if entries[0].TransactionCode-5 != entries[1].TransactionCode {
		t.Errorf("entries[0].TransactionCode=%d entries[1].TransactionCode=%d", entries[0].TransactionCode, entries[1].TransactionCode)
	}
	if entries[0].Amount != 12 {
		t.Errorf("entries[0].Amount=%d", entries[0].Amount)
	}
	if entries[1].Amount != 14 {
		t.Errorf("entries[1].Amount=%d", entries[1].Amount)
	}
	if entries[1].TraceNumber != "124" {
		t.Errorf("entries[1].TraceNumber=%s", entries[1].TraceNumber)
	}
}

func TestMicroDeposits_submitMicroDeposits(t *testing.T) {
	w := httptest.NewRecorder()

	achClient, _, achServer := achclient.MockClientServer("submitMicroDeposits", func(r *mux.Router) {
		achclient.AddCreateRoute(w, r)
		achclient.AddValidateRoute(r)
	})
	defer achServer.Close()

	testODFIAccount := makeTestODFIAccount()

	router := &DepositoryRouter{
		logger:      log.NewNopLogger(),
		achClient:   achClient,
		eventRepo:   &testEventRepository{},
		odfiAccount: testODFIAccount,
	}
	router.accountsClient = nil // explicitly disable Accounts calls for this test
	userID, requestID := base.ID(), base.ID()

	amounts := []Amount{
		{symbol: "USD", number: 12}, // $0.12
		{symbol: "USD", number: 37}, // $0.37
	}

	dep := &Depository{
		ID:            DepositoryID(base.ID()),
		BankName:      "bank name",
		Holder:        "holder",
		HolderType:    Individual,
		Type:          Checking,
		RoutingNumber: "121042882",
		AccountNumber: "151",
		Status:        DepositoryUnverified,
	}

	microDeposits, err := router.submitMicroDeposits(userID, requestID, amounts, dep)
	if n := len(microDeposits); n != 2 || err != nil {
		t.Fatalf("n=%d error=%v", n, err)
	}
}

func TestMicroDeposits__lookupMicroDepositFromReturn(t *testing.T) {
	t.Parallel()

	check := func(t *testing.T, repo *SQLDepositoryRepo) {
		amt1, _ := NewAmount("USD", "0.11")
		amt2, _ := NewAmount("USD", "0.12")

		userID := base.ID()
		depID1, depID2 := DepositoryID(base.ID()), DepositoryID(base.ID())

		// initial lookups with no rows written
		if md, err := repo.lookupMicroDepositFromReturn(depID1, amt1); md != nil || err != nil {
			t.Errorf("micro-deposit=%#v error=%v", md, err)
		}
		if md, err := repo.lookupMicroDepositFromReturn(depID1, amt2); md != nil || err != nil {
			t.Errorf("micro-deposit=%#v error=%v", md, err)
		}
		if md, err := repo.lookupMicroDepositFromReturn(depID2, amt1); md != nil || err != nil {
			t.Errorf("micro-deposit=%#v error=%v", md, err)
		}
		if md, err := repo.lookupMicroDepositFromReturn(depID2, amt2); md != nil || err != nil {
			t.Errorf("micro-deposit=%#v error=%v", md, err)
		}

		// write a micro-deposit and then lookup
		microDeposits := []*microDeposit{
			{amount: *amt1, fileID: "fileID", transactionID: "transactionID"},
			{amount: *amt2, fileID: "fileID2", transactionID: "transactionID2"},
		}
		if err := repo.initiateMicroDeposits(depID1, userID, microDeposits); err != nil {
			t.Fatal(err)
		}

		// lookups (matching cases)
		if md, err := repo.lookupMicroDepositFromReturn(depID1, amt1); md == nil || err != nil {
			t.Errorf("micro-deposit=%#v error=%v", md, err)
		}
		if md, err := repo.lookupMicroDepositFromReturn(depID1, amt2); md == nil || err != nil {
			t.Errorf("micro-deposit=%#v error=%v", md, err)
		}

		// lookups (not matching cases)
		if md, err := repo.lookupMicroDepositFromReturn(depID2, amt1); md != nil || err != nil {
			t.Errorf("micro-deposit=%#v error=%v", md, err)
		}
		if md, err := repo.lookupMicroDepositFromReturn(depID2, amt2); md != nil || err != nil {
			t.Errorf("micro-deposit=%#v error=%v", md, err)
		}
	}

	// SQLite tests
	sqliteDB := database.CreateTestSqliteDB(t)
	defer sqliteDB.Close()
	check(t, &SQLDepositoryRepo{sqliteDB.DB, log.NewNopLogger()})

	// MySQL tests
	mysqlDB := database.CreateTestMySQLDB(t)
	defer mysqlDB.Close()
	check(t, &SQLDepositoryRepo{mysqlDB.DB, log.NewNopLogger()})
}

func getReturnCode(t *testing.T, db *sql.DB, depID DepositoryID, amt *Amount) string {
	t.Helper()

	query := `select return_code from micro_deposits where depository_id = ? and amount = ? and deleted_at is null`
	stmt, err := db.Prepare(query)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()

	var returnCode string
	if err := stmt.QueryRow(depID, amt.String()).Scan(&returnCode); err != nil {
		if err == sql.ErrNoRows {
			return ""
		}
		t.Fatal(err)
	}
	return returnCode
}

func TestMicroDeposits__setReturnCode(t *testing.T) {
	t.Parallel()

	check := func(t *testing.T, repo *SQLDepositoryRepo) {
		amt, _ := NewAmount("USD", "0.11")
		depID, userID := DepositoryID(base.ID()), base.ID()

		// get an empty return_code as we've written nothing
		if code := getReturnCode(t, repo.db, depID, amt); code != "" {
			t.Fatalf("code=%s", code)
		}

		// write a micro-deposit and set the return code
		microDeposits := []*microDeposit{
			{amount: *amt, fileID: "fileID", transactionID: "transactionID"},
		}
		if err := repo.initiateMicroDeposits(depID, userID, microDeposits); err != nil {
			t.Fatal(err)
		}
		if err := repo.setReturnCode(depID, *amt, "R14"); err != nil {
			t.Fatal(err)
		}

		// lookup again and expect the return_code
		if code := getReturnCode(t, repo.db, depID, amt); code != "R14" {
			t.Errorf("code=%s", code)
		}

		xs, err := repo.getMicroDepositsForUser(depID, userID)
		t.Logf("xs=%#v error=%v", xs[0], err)
	}

	// SQLite tests
	sqliteDB := database.CreateTestSqliteDB(t)
	defer sqliteDB.Close()
	check(t, &SQLDepositoryRepo{sqliteDB.DB, log.NewNopLogger()})

	// MySQL tests
	mysqlDB := database.CreateTestMySQLDB(t)
	defer mysqlDB.Close()
	check(t, &SQLDepositoryRepo{mysqlDB.DB, log.NewNopLogger()})
}
