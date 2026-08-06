package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getIDs(t *testing.T, h http.Handler, room string) (int, []string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/rooms/"+room+"/ids", nil))
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	return rec.Code, body.IDs
}

func TestIDsEndpointListsWhatTheRoomHolds(t *testing.T) {
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}
	h := s.Routes(stubStatic())

	a := publish(room, "one")
	b := publish(room, "two")

	code, ids := getIDs(t, h, room.ID)
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(ids) != 2 || ids[0] != a.ID || ids[1] != b.ID {
		t.Fatalf("ids = %v, want [%s %s]", ids, a.ID, b.ID)
	}

	// And it reflects a deletion, which is the entire reason it exists: a tab
	// that slept through this has no other way to find out.
	room.Delete(a.ID)
	_, ids = getIDs(t, h, room.ID)
	if len(ids) != 1 || ids[0] != b.ID {
		t.Errorf("after deleting %s, ids = %v, want [%s]", a.ID, ids, b.ID)
	}
}

// The answer has to be small -- that is the whole justification for a second
// endpoint rather than refetching the room. A full room of maximum-size items
// must still come back in a couple of kilobytes.
func TestIDsEndpointStaysSmallForAFullRoom(t *testing.T) {
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}
	h := s.Routes(stubStatic())

	big := strings.Repeat("x", 200<<10)
	for range 50 {
		publish(room, big)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/rooms/"+room.ID+"/ids", nil))

	if n := rec.Body.Len(); n > 4<<10 {
		t.Errorf("a full room's id list is %d bytes; the point of this endpoint is that it is small", n)
	}
	if strings.Contains(rec.Body.String(), "xxxx") {
		t.Error("content leaked into the id list")
	}
}

func TestIDsEndpointIsA404ForAnUnknownRoom(t *testing.T) {
	s := newTestServer(t)
	if code, _ := getIDs(t, s.Routes(stubStatic()), "nosuchroomatall"); code != http.StatusNotFound {
		t.Errorf("status %d, want 404", code)
	}
}

// An empty room must answer with an empty list rather than null, or the client
// iterates over nothing and silently reconciles against nothing.
func TestIDsEndpointAnswersEmptyNotNull(t *testing.T) {
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.Routes(stubStatic()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/rooms/"+room.ID+"/ids", nil))

	if got := strings.TrimSpace(rec.Body.String()); got != `{"ids":[]}` {
		t.Errorf("body = %s, want {\"ids\":[]}", got)
	}
}

// Reads are never rate limited. A phone waking up and reconciling is the
// product working correctly, and this runs on every visibilitychange.
func TestIDsEndpointIsNotRateLimited(t *testing.T) {
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}
	h := s.Routes(stubStatic())

	// Comfortably past the 60/min write budget.
	for i := range 120 {
		if code, _ := getIDs(t, h, room.ID); code != http.StatusOK {
			t.Fatalf("request %d gave %d; reads must not be metered", i+1, code)
		}
	}
}
