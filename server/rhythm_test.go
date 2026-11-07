package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestScoreRunMatchesBadge(t *testing.T) {
	cases := []struct {
		n        int
		counts   [4]int
		combo    int
		score    int
		acc      float64
		grade    string
		wantFail bool
	}{
		{n: 20, counts: [4]int{20, 0, 0, 0}, combo: 20, score: 1000000, acc: 1, grade: "S"},
		// 5550 of 6000 points: 900000*5550/6000 + 100000*20/20 = 832500 + 100000
		{n: 20, counts: [4]int{16, 2, 2, 0}, combo: 20, score: 932500, acc: 5550.0 / 6000, grade: "A"},
		{n: 23, counts: [4]int{11, 6, 6, 0}, combo: 23, score: 823913, acc: 5550.0 / 6900, grade: "B"},
		{n: 20, counts: [4]int{19, 0, 0, 1}, combo: 12, score: 915000, acc: 0.95, grade: "S"},
		{n: 3, counts: [4]int{1, 1, 0, 1}, combo: 1, score: 558333, acc: 525.0 / 900, grade: "D"},
		{n: 20, counts: [4]int{10, 0, 0, 0}, combo: 10, wantFail: true},  // counts short of the chart
		{n: 20, counts: [4]int{20, 0, 0, 0}, combo: 19, wantFail: true},  // no misses, so combo is every note
		{n: 20, counts: [4]int{18, 0, 0, 2}, combo: 19, wantFail: true},  // more combo than hits
		{n: 20, counts: [4]int{18, 0, 0, 2}, combo: 5, wantFail: true},   // 18 hits in 3 streaks need one of 6+
		{n: 20, counts: [4]int{21, 0, 0, -1}, combo: 20, wantFail: true}, // negative count
	}
	for _, c := range cases {
		got, err := scoreRun(c.n, c.counts, c.combo)
		if c.wantFail {
			if err == nil {
				t.Errorf("%v combo %d: accepted, want error", c.counts, c.combo)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v combo %d: %v", c.counts, c.combo, err)
			continue
		}
		if got.Score != c.score || got.Grade != c.grade || got.Acc-c.acc > 1e-9 || c.acc-got.Acc > 1e-9 {
			t.Errorf("%v combo %d = %+v, want score %d acc %v grade %s", c.counts, c.combo, got, c.score, c.acc, c.grade)
		}
	}
}

func TestCheckClaimed(t *testing.T) {
	want, _ := scoreRun(20, [4]int{16, 2, 2, 0}, 20)
	if err := checkClaimed(want, want.Score-1, want.Acc+5e-5, want.Grade); err != nil {
		t.Errorf("float32 rounding rejected: %v", err)
	}
	for _, bad := range []struct {
		score int
		acc   float64
		grade string
	}{
		{want.Score + 2, want.Acc, want.Grade},
		{want.Score, want.Acc + 0.01, want.Grade},
		{want.Score, want.Acc, "S"},
	} {
		if checkClaimed(want, bad.score, bad.acc, bad.grade) == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestCheckName(t *testing.T) {
	for in, want := range map[string]string{"  orpheus  ": "orpheus", "Heidi  Hacker": "Heidi Hacker", "zoë_99": "zoë_99"} {
		if got, err := checkName(in); err != nil || got != want {
			t.Errorf("checkName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "   ", "seventeen chars!!", "a<script>", "sh1t head", "F.U.C.K"} {
		if _, err := checkName(in); err == nil {
			t.Errorf("checkName(%q) accepted", in)
		}
	}
}

func TestDemoSongsParse(t *testing.T) {
	songs, err := demoSongs()
	if err != nil || len(songs) != 2 {
		t.Fatalf("demo songs %d, err %v", len(songs), err)
	}
	for _, s := range songs {
		for _, d := range diffs {
			if s.Notes[d] == 0 {
				t.Errorf("%s has no %s notes", s.ID, d)
			}
		}
	}
	for _, chart := range []string{`[[0,4]]`, `[[0,1,-5]]`, `[[0,1,100,2]]`, `[[900,1,20000]]`} {
		if _, err := parseSong("bad", []byte(`{"title":"x","bpm":120,"duration_ms":1000,"charts":{"easy":`+chart+`}}`)); err == nil {
			t.Errorf("chart %s accepted", chart)
		}
	}
	s, err := parseSong("holds", []byte(`{"title":"x","bpm":120,"duration_ms":1000,"charts":{"easy":[[500,1],[0,2,300],[700,3,0]]}}`))
	if err != nil || s.Notes["easy"] != 4 || len(s.Charts["easy"][0]) != 3 {
		t.Fatalf("holds: %+v %v, want 4 judgments with the hold sorted first", s, err)
	}
}

// memRhythm is a Rhythm with the demo songs and no database, for room tests.
func memRhythm(t *testing.T, now *time.Time) *Rhythm {
	t.Helper()
	r, err := newRhythm(nil, "https://quadrant.underwhelm.ing")
	if err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return *now }
	songs, err := demoSongs()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range songs {
		r.songs[s.ID] = s
	}
	return r
}

func TestRoomLifecycle(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := memRhythm(t, &now)
	code, token, err := r.createRoom()
	if err != nil || len(code) != 4 || strings.ContainsAny(code, "01OIL") {
		t.Fatalf("room %q, err %v", code, err)
	}
	ch, state, cancel, err := r.subscribe(strings.ToLower(code))
	if err != nil || state.T != "idle" {
		t.Fatalf("subscribe: %+v %v", state, err)
	}
	defer cancel()
	start := func(st *RoomState) error {
		*st = RoomState{T: "start", Song: "demo100", Diff: "hard", StartAt: 5}
		return nil
	}
	if _, err := r.command(code, "wrong", start); err != errBadToken {
		t.Fatalf("wrong token: %v", err)
	}
	if n, err := r.command(code, token, start); err != nil || n != 1 {
		t.Fatalf("start: %d listeners, %v", n, err)
	}
	r.command(code, token, func(st *RoomState) error { st.T = "pause"; return nil })
	if got := <-ch; got.T != "pause" || got.Seq != 2 || got.Song != "demo100" {
		t.Fatalf("subscriber got %+v, want only the latest state", got)
	}

	now = now.Add(roomTTL - time.Second)
	if _, _, _, err := r.roomInfo(code); err != nil {
		t.Fatalf("room gone before ttl: %v", err)
	}
	now = now.Add(roomTTL + time.Second)
	if _, _, _, err := r.roomInfo(code); err != errNoRoom {
		t.Fatalf("expired room: %v", err)
	}
	if _, ok := <-ch; ok {
		t.Fatal("subscriber channel still open after expiry")
	}
}

// rhythmServer is the full HTTP stack with a test database and a movable clock.
func rhythmServer(t *testing.T) (http.Handler, *Rhythm, *time.Time) {
	t.Helper()
	s := openTestStore(t)
	r, err := OpenRhythm(s.db, "https://quadrant.underwhelm.ing")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.now = func() time.Time { return now }
	// t20: 18 taps and one hold on easy, so 19 entries and 20 judgments.
	chart := `[[1000,0,400]`
	for i := 1; i < 19; i++ {
		chart += fmt.Sprintf(",[%d,%d]", 1000+i*500, i%4)
	}
	fixture, err := parseSong("t20", []byte(`{"title":"T20","artist":"Test","bpm":120,"duration_ms":12000,"charts":{"easy":`+chart+`]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.saveSong(fixture, nil, ""); err != nil {
		t.Fatal(err)
	}
	srv := &Server{store: s, hub: NewHub(), limiter: NewLimiter(0), rhythm: r, adminPassword: "pw"}
	return srv.Routes(), r, &now
}

func call(t *testing.T, h http.Handler, method, url, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func runJSON(counts [4]int, combo int, name string) string {
	sc, err := scoreRun(20, counts, combo)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"song":"t20","diff":"easy","score":%d,"acc":%v,"grade":%q,"counts":[%d,%d,%d,%d],"max_combo":%d,"notes":20%s}`,
		sc.Score, sc.Acc, sc.Grade, counts[0], counts[1], counts[2], counts[3], combo, name)
}

func TestRunClaimFlow(t *testing.T) {
	h, _, now := rhythmServer(t)

	for _, bad := range []string{
		// counts cover the 19 entries but not the hold's tail
		`{"song":"t20","diff":"easy","score":1000000,"acc":1,"grade":"S","counts":[19,0,0,0],"max_combo":19,"notes":19}`,
		`{"song":"t20","diff":"easy","score":999999,"acc":0.9,"grade":"S","counts":[20,0,0,0],"max_combo":20,"notes":20}`,
		`{"song":"t20","diff":"expert","score":1000000,"acc":1,"grade":"S","counts":[20,0,0,0],"max_combo":20,"notes":20}`,
		`{"song":"t20","diff":"easy","score":1000000,"acc":1,"grade":"S","counts":[20,0,0,0],"max_combo":20,"notes":19}`,
	} {
		if code, out := call(t, h, "POST", "/api/rhythm/runs", bad); code != 400 {
			t.Errorf("POST %s = %d %v, want 400", bad, code, out)
		}
	}

	claim := func(counts [4]int, combo int) string {
		code, out := call(t, h, "POST", "/api/rhythm/runs", runJSON(counts, combo, ""))
		if code != 201 || !strings.HasPrefix(out["url"].(string), "https://quadrant.underwhelm.ing/c/") {
			t.Fatalf("POST run = %d %v", code, out)
		}
		rows, _ := out["qr"].([]any)
		if len(rows) < 21 || len(rows[0].(string)) != len(rows) {
			t.Fatalf("qr = %v", out["qr"])
		}
		return out["code"].(string)
	}
	first := claim([4]int{16, 2, 2, 0}, 20)
	if code, out := call(t, h, "GET", "/api/rhythm/claims/"+strings.ToLower(first), ""); code != 200 || out["grade"] != "A" || out["title"] != "T20" {
		t.Fatalf("GET claim = %d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/api/rhythm/claims/"+first, `{"name":"a<b>"}`); code != 400 {
		t.Fatalf("bad name = %d, want 400", code)
	}
	if code, out := call(t, h, "POST", "/api/rhythm/claims/"+first, `{"name":" orpheus "}`); code != 200 || out["name"] != "orpheus" {
		t.Fatalf("claim = %d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/api/rhythm/claims/"+first, `{"name":"thief"}`); code != 409 {
		t.Fatalf("second claim = %d, want 409", code)
	}

	worse := claim([4]int{10, 5, 5, 0}, 20)
	call(t, h, "POST", "/api/rhythm/claims/"+worse, `{"name":"Orpheus"}`)
	other := claim([4]int{20, 0, 0, 0}, 20)
	call(t, h, "POST", "/api/rhythm/claims/"+other, `{"name":"heidi"}`)

	code, out := call(t, h, "GET", "/api/rhythm/leaderboard?song=t20&diff=easy", "")
	runs, _ := out["runs"].([]any)
	if code != 200 || len(runs) != 2 {
		t.Fatalf("leaderboard = %d %v, want heidi and orpheus's best", code, out)
	}
	if top := runs[0].(map[string]any); top["name"] != "heidi" || runs[1].(map[string]any)["score"].(float64) != 932500 {
		t.Fatalf("leaderboard order %v", runs)
	}

	stale := claim([4]int{20, 0, 0, 0}, 20)
	*now = now.Add(claimTTL + time.Minute)
	if code, _ := call(t, h, "GET", "/api/rhythm/claims/"+stale, ""); code != 404 {
		t.Fatalf("expired claim GET = %d, want 404", code)
	}
	if code, _ := call(t, h, "POST", "/api/rhythm/claims/"+stale, `{"name":"late"}`); code != 404 {
		t.Fatalf("expired claim POST = %d, want 404", code)
	}
}

func TestRoomOverHTTP(t *testing.T) {
	h, r, _ := rhythmServer(t)
	ts := httptest.NewServer(h)
	defer ts.Close()

	code, room := call(t, h, "POST", "/api/rhythm/rooms", "")
	if code != 201 {
		t.Fatalf("create room = %d %v", code, room)
	}
	rc, token := room["code"].(string), room["token"].(string)

	resp, err := http.Get(ts.URL + "/api/rhythm/rooms/" + rc + "/events")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("events: %v %v", resp, err)
	}
	defer resp.Body.Close()
	events := bufio.NewScanner(resp.Body)
	next := func() RoomState {
		for events.Scan() {
			if line, ok := strings.CutPrefix(events.Text(), "data: "); ok {
				var st RoomState
				json.Unmarshal([]byte(line), &st)
				return st
			}
		}
		t.Fatal("event stream ended")
		return RoomState{}
	}
	if st := next(); st.T != "idle" {
		t.Fatalf("first event %+v", st)
	}

	startAt := r.serverMS() + 3000
	if code, out := call(t, h, "POST", "/api/rhythm/rooms/"+rc+"/pause", `{"token":"`+token+`"}`); code != 400 {
		t.Fatalf("pause before start = %d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/api/rhythm/rooms/"+rc+"/start", fmt.Sprintf(`{"token":"nope","song":"demo100","diff":"hard","start_at":%f}`, startAt)); code != 403 {
		t.Fatalf("start with wrong token = %d", code)
	}
	code, out := call(t, h, "POST", "/api/rhythm/rooms/"+rc+"/start", fmt.Sprintf(`{"token":%q,"song":"demo100","diff":"hard","start_at":%f}`, token, startAt))
	if code != 200 || out["listeners"].(float64) != 1 {
		t.Fatalf("start = %d %v", code, out)
	}
	if st := next(); st.T != "start" || st.Song != "demo100" || st.StartAt != startAt {
		t.Fatalf("start event %+v", st)
	}

	run := strings.Replace(runJSON([4]int{20, 0, 0, 0}, 20, ""), `{`, fmt.Sprintf(`{"room":%q,"token":%q,`, rc, token), 1)
	if code, out := call(t, h, "POST", "/api/rhythm/runs", run); code != 201 {
		t.Fatalf("run = %d %v", code, out)
	}
	if st := next(); st.T != "result" || st.Grade != "S" || !strings.Contains(st.Claim, "/c/") {
		t.Fatalf("result event %+v", st)
	}
}

func TestHostRoutingAndSongs(t *testing.T) {
	h, _, _ := rhythmServer(t)
	get := func(host, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if body := get("quadrant.underwhelm.ing", "/").Body.String(); !strings.Contains(body, "<title>Quadrant</title>") {
		t.Fatal("quadrant host did not serve the quadrant page")
	}
	if body := get("quadrant.localhost:8080", "/play").Body.String(); !strings.Contains(body, "play.js") {
		t.Fatal("quadrant.localhost did not serve the player")
	}
	if body := get("ink.underwhelm.ing", "/").Body.String(); !strings.Contains(body, "<title>ink</title>") {
		t.Fatal("ink host changed")
	}
	rec := get("ink.underwhelm.ing", "/api/rhythm/songs/demo128")
	var song Song
	json.Unmarshal(rec.Body.Bytes(), &song)
	if rec.Code != 200 || rec.Header().Get("Content-Length") == "" || judgments(song.Charts["hard"]) != song.Notes["hard"] || song.BPM != 128 {
		t.Fatalf("song = %d %s", rec.Code, rec.Header())
	}
}

func TestAdminUploadSong(t *testing.T) {
	h, _, _ := rhythmServer(t)
	upload := func(pass string, audio []byte) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		f, _ := mw.CreateFormFile("song", "song.json")
		f.Write([]byte(`{"title":"Test Song!","artist":"Me","bpm":120,"offset_ms":40,"duration_ms":4000,"charts":{"normal":[[1000,0],[500,3]]}}`))
		if audio != nil {
			f, _ = mw.CreateFormFile("audio", "a.mp3")
			f.Write(audio)
		}
		mw.Close()
		req := httptest.NewRequest("POST", "/api/admin/rhythm/songs", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.SetBasicAuth("", pass)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := upload("wrong", nil); rec.Code != 401 {
		t.Fatalf("no auth = %d", rec.Code)
	}
	if rec := upload("pw", []byte("not audio")); rec.Code != 400 {
		t.Fatalf("bad audio = %d", rec.Code)
	}
	mp3 := append([]byte("ID3"), make([]byte, 100)...)
	if rec := upload("pw", mp3); rec.Code != 200 {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	if rec := upload("pw", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"has_audio":true`) {
		t.Fatalf("re-upload without audio lost it: %s", rec.Body)
	}
	code, out := call(t, h, "GET", "/api/rhythm/songs/test-song", "")
	if charts := out["charts"].(map[string]any)["normal"].([]any); code != 200 || charts[0].([]any)[0].(float64) != 500 {
		t.Fatalf("song = %d %v, want notes sorted", code, out)
	}
	req := httptest.NewRequest("GET", "/api/rhythm/songs/test-song/audio", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "audio/mpeg" || !bytes.Equal(rec.Body.Bytes(), mp3) {
		t.Fatalf("audio = %d %s", rec.Code, rec.Header())
	}
}
