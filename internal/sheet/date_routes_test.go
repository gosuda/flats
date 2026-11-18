package sheet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// download fetches a tab as CSV over HTTP, as a browser or a script does.
func (s *site) download(route string, tab TabID) (int, http.Header, string) {
	s.e.t.Helper()
	res, err := http.Get(fmt.Sprintf("%s/%s/_sheet/csv?tab=%d", s.server.URL, route, tab))
	if err != nil {
		s.e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(body)
}

// TestDownloadFollowsTheDate downloads a tab as CSV the day after its
// TODAY formula was calculated, with no connection open: from the
// instance that stayed loaded across midnight, and from one that loads
// the sheet again with its stored date. Each download has the new date.
func TestDownloadFollowsTheDate(t *testing.T) {
	for _, reload := range []bool{false, true} {
		name := "loaded across midnight"
		if reload {
			name = "loaded again"
		}
		t.Run(name, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=TODAY()-DATE(2026,11,13)"]]}]}`)
			s := serve(e)
			tab := e.tabID("Sheet1")
			day := e.calcDate()
			if code, _, body := s.download("private", tab); code != http.StatusOK || !strings.HasSuffix(body, "0\r\n") {
				t.Fatalf("the download on the first day: status %d, %q", code, body)
			}
			e.clock.Add(24 * time.Hour)
			if reload {
				e.restart()
			}
			if got := e.calcDate(); got != day {
				t.Fatalf("the sheet is at %s before the download, want %s", got, day)
			}
			for _, route := range []string{"private", "public"} {
				code, _, body := s.download(route, tab)
				if code != http.StatusOK || !strings.HasSuffix(body, "1\r\n") {
					t.Fatalf("the %s download the next day: status %d, %q", route, code, body)
				}
			}
			if e.calcDate() == day {
				t.Fatal("the download did not move the sheet to the new date")
			}
		})
	}
}

// TestDownloadDuringADateChange downloads a tab while the date change it
// asks for is long work, with no connection open. The download is refused
// at once as calculating, the change goes on, and the next download after
// it has the new values.
func TestDownloadDuringADateChange(t *testing.T) {
	e := longSheet(t)
	s := serve(e)
	tab := e.tabID("Sheet1")
	held := holdPasses(t)
	held.hold(e.in)
	guard := time.AfterFunc(10*shortBound, held.release)
	defer guard.Stop()
	e.clock.Add(24 * time.Hour)
	for i, what := range []string{"the download that meets the new date", "a download during the change"} {
		timed(t, what, func() {
			code, header, body := s.download("private", tab)
			var out WriteError
			_ = json.Unmarshal([]byte(body), &out)
			if code != http.StatusServiceUnavailable || out.Reason != "calculating" || !out.Retry || header.Get("Retry-After") == "" {
				t.Fatalf("%s: status %d, %.80q", what, code, body)
			}
		})
		if i == 0 {
			held.reached(t, 1)
		}
	}
	held.release()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		code, _, body := s.download("private", tab)
		if code == http.StatusOK {
			if !strings.Contains(body, "1,"+longSum+","+longSum+"\r\n") {
				t.Fatalf("the download after the date change: %.80q", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the download after the date change: status %d, %.80q", code, body)
		}
	}
}

// TestRecoveryFollowsTheDate recovers a tab from History the day after
// the last edit, with no connection open. The sheet moves to the new date
// before the recovery is answered.
func TestRecoveryFollowsTheDate(t *testing.T) {
	e := start(t)
	e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=TODAY()-DATE(2026,11,13)"]]}]}`)
	rev := e.b.importAll(t)
	s := serve(e)
	day := e.calcDate()
	e.clock.Add(24 * time.Hour)
	req, _ := http.NewRequest(http.MethodPost, s.server.URL+"/private/_sheet/api/recover", strings.NewReader(fmt.Sprintf(`{"revision":%d,"tab":%d}`, rev, e.tabID("Sheet1"))))
	req.Header.Set("X-Flats-Sheet", "1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the recovery: status %d, %s", res.StatusCode, body)
	}
	if got := e.calcDate(); got == day {
		t.Fatalf("the recovery was answered at %s, the previous date", got)
	}
	e.want("'Sheet1 (restored)'!A1", "1")
}

// TestFailedDateChangeIsRefused moves the clock past midnight with no
// connection open while every commit of the sheet fails, and asks for the
// CSV download and for a fresh agent read. The date change does not
// commit, so both are refused as unavailable, to be tried again, and the
// sheet keeps its previous date and values. Once commits succeed again the
// next attempt has the new date.
func TestFailedDateChangeIsRefused(t *testing.T) {
	for _, how := range []string{"download", "read"} {
		t.Run(how, func(t *testing.T) {
			e := start(t)
			e.mustAgent(`{"ops":[{"op":"write","tab":"Sheet1","at":"A1","rows":[["=TODAY()-DATE(2026,11,13)"]]}]}`)
			s := serve(e)
			tab := e.tabID("Sheet1")
			day := e.calcDate()
			e.in.mu.Lock()
			e.in.fault = func() error { return errors.New("injected commit failure") }
			e.in.mu.Unlock()
			e.clock.Add(24 * time.Hour)
			ask := func() (int, string, error) {
				if how == "download" {
					code, _, body := s.download("private", tab)
					return code, body, nil
				}
				data, err := e.in.Read(context.Background(), ReadQuery{Tab: "Sheet1"})
				return 0, string(data), err
			}
			for range 2 {
				code, body, err := ask()
				var out WriteError
				if how == "download" {
					_ = json.Unmarshal([]byte(body), &out)
				} else if err != nil {
					out = *refusal(err)
				}
				if (how == "download" && code != http.StatusServiceUnavailable) || out.Code != "unavailable" || out.Reason != "commit" || !out.Retry {
					t.Fatalf("with the date change failing: status %d, %v, %.80q", code, err, body)
				}
				if got := e.calcDate(); got != day {
					t.Fatalf("the failed change left the sheet at %s, want %s", got, day)
				}
			}
			e.want("Sheet1!A1", "0")
			e.in.mu.Lock()
			e.in.fault = nil
			e.in.mu.Unlock()
			code, body, err := ask()
			switch how {
			case "download":
				if code != http.StatusOK || !strings.HasSuffix(body, "1\r\n") {
					t.Fatalf("the download once commits succeed: status %d, %q", code, body)
				}
			case "read":
				var out map[string]any
				if err != nil || json.Unmarshal([]byte(body), &out) != nil {
					t.Fatalf("the read once commits succeed: %v, %.80q", err, body)
				}
				if out["date"] == day || !strings.Contains(body, `"value":1}`) {
					t.Fatalf("the read once commits succeed: %.300s", body)
				}
			}
			if e.calcDate() == day {
				t.Fatal("the sheet did not move to the new date")
			}
			e.want("Sheet1!A1", "1")
		})
	}
}
