package core

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gosuda/flats/internal/store"
)

func TestPublicRouteStopRevokesEpochAndReExposureRenewsIt(t *testing.T) {
	s, _ := newTestService(t)
	_, err := s.CreateFlat(t.Context(), "epoch", "Epoch", ViaMCP)
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.st.GetFlat(t.Context(), "epoch")
	if err != nil {
		t.Fatal(err)
	}
	f.Visibility = store.Public
	if err := s.st.UpdateFlat(t.Context(), f); err != nil {
		t.Fatal(err)
	}
	started, ended := make(chan struct{}), make(chan struct{})
	lf := s.state("epoch")
	lf.cur.Store(&deployed{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(ended)
	})})
	old := s.siteHandler("epoch", true)
	go old.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	if err := s.stopPublicRoutes(t.Context(), "epoch"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("route stop did not cancel admitted request")
	}
	lf.cur.Store(&deployed{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })})
	for _, tt := range []struct {
		h      http.Handler
		status int
	}{{old, 503}, {s.siteHandler("epoch", true), 204}, {s.siteHandler("epoch", false), 204}} {
		rec := httptest.NewRecorder()
		tt.h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != tt.status {
			t.Fatal("epoch admission", rec.Code, tt.status)
		}
	}
}
