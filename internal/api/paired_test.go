package api

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Redeeming a code has to reach the device that offered it. That device is
// holding a QR and a countdown, and the stream is the only thing the server can
// reach it on -- so if this event does not arrive, "did the scan work?" has no
// answer and the sheet just sits there.

func TestRedeemingACodeNotifiesTheRoom(t *testing.T) {
	s := newTestServer(t)
	room, err := s.Hub.Create()
	if err != nil {
		t.Fatal(err)
	}
	h := s.Routes(stubStatic())
	ts := httptest.NewServer(h)
	defer ts.Close()

	// The offering device, holding the stream open.
	req, err := http.NewRequest("GET", ts.URL+"/api/rooms/"+room.ID+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	events := make(chan string, 8)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "data:") {
				events <- line
			}
		}
	}()

	// Give the subscription a moment to land, or the broadcast has nobody to
	// reach -- fan-out is non-blocking by design.
	time.Sleep(100 * time.Millisecond)

	mint := postPair(t, h, `{"room":"`+room.ID+`","code":"K3N8XQ2M","payload":"c2FsdA.aXY.Y3Q"}`)
	if mint.Code != http.StatusCreated {
		t.Fatalf("mint gave %d", mint.Code)
	}

	// The joining device redeems, announcing itself with a recognisable UA.
	redeem, err := http.NewRequest("POST", ts.URL+"/api/pair/K3N8-XQ2M", nil)
	if err != nil {
		t.Fatal(err)
	}
	redeem.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) Version/17.0 Safari/605.1")
	rr, err := http.DefaultClient.Do(redeem)
	if err != nil {
		t.Fatal(err)
	}
	rr.Body.Close()
	if rr.StatusCode != http.StatusOK {
		t.Fatalf("redeem gave %d", rr.StatusCode)
	}

	var sawEvent, sawData bool
	deadline := time.After(3 * time.Second)
	for !(sawEvent && sawData) {
		select {
		case line := <-events:
			switch {
			case line == "event: paired":
				sawEvent = true
			case strings.HasPrefix(line, "data:") && strings.Contains(line, "iPhone"):
				sawData = true
				// Cosmetic label only. If this ever carries anything else, the
				// event has grown a payload it has no business having.
				if !strings.Contains(line, "Safari") {
					t.Errorf("origin did not survive: %q", line)
				}
			}
		case <-deadline:
			t.Fatalf("no paired event reached the stream (event=%v data=%v)", sawEvent, sawData)
		}
	}
}

// A paired event must not carry an id:. Advancing Last-Event-ID onto a
// non-item would make the next reconnect skip real items.
func TestPairedEventCarriesNoEventID(t *testing.T) {
	var sb strings.Builder
	rec := &flushRecorder{ResponseRecorder: httptest.NewRecorder(), body: &sb}
	if err := writePaired(rec, "iPhone · Safari"); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if strings.Contains(got, "id:") {
		t.Fatalf("paired event carries an id, which would poison Last-Event-ID:\n%s", got)
	}
	if !strings.HasPrefix(got, "event: paired\n") {
		t.Fatalf("unexpected framing:\n%s", got)
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	body *strings.Builder
}

func (f *flushRecorder) Write(p []byte) (int, error) { return f.body.Write(p) }
