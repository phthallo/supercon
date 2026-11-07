package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/cmplx"
	"os"
	"sort"

	"github.com/hajimehoshi/go-mp3"
)

const (
	bmRate     = 11025 // analysis rate after downsampling
	bmWin      = 1024
	bmHop      = 128
	bmMaxMS    = 10 * 60 * 1000
	bmLeadInMS = 3000 // the first note needs time to travel down its track
)

type beatSpec struct {
	diff      string
	perBeat   int     // grid slots per beat a note may start on
	minGapMS  float64 // between any two notes
	keep      float64 // fraction of grid slots, strongest first, that may become notes
	chords    bool
	holdBeats []int
}

var beatSpecs = []beatSpec{
	{"easy", 1, 1500, 0.35, false, []int{2}},
	{"normal", 2, 850, 0.45, true, []int{2, 3}},
	{"hard", 2, 420, 0.6, true, []int{2, 3, 4}},
}

// onsets is the per-hop analysis of a track.
type onsets struct {
	hopMS     float64
	flux      []float64 // onset strength, 0-1
	low, high []float64 // onset strength below 200 Hz and above 2 kHz
	rms       []float64
}

// GenerateSong builds easy, normal and hard charts for an mp3 from its beat grid and onsets.
func GenerateSong(id, title, artist string, data []byte) (*Song, error) {
	mono, rate, err := decodeMono(data)
	if err != nil {
		return nil, err
	}
	durMS := len(mono) * 1000 / rate
	if durMS < 20000 {
		return nil, errors.New("audio is shorter than 20 s")
	}
	o := analyse(mono, rate)
	bpm, phase := o.tempo()
	beat := 60000 / bpm
	s := &Song{ID: id, Title: title, Artist: artist, BPM: math.Round(bpm*100) / 100,
		OffsetMS: int(math.Round(phase)), DurationMS: durMS, Charts: map[string][]Note{}}
	for _, spec := range beatSpecs {
		s.Charts[spec.diff] = o.chart(spec, beat, phase, float64(durMS))
	}
	s.countNotes()
	return s, nil
}

// beatmapCmd writes a song file for the badge: ink beatmap AUDIO.mp3 TITLE [ARTIST] > song.json
func beatmapCmd(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ink beatmap AUDIO.mp3 TITLE [ARTIST] > song.json")
		os.Exit(2)
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		log.Fatal(err)
	}
	artist := ""
	if len(args) > 2 {
		artist = args[2]
	}
	s, err := GenerateSong(slugify(args[1]), args[1], artist, data)
	if err != nil {
		log.Fatal(err)
	}
	out, _ := json.Marshal(map[string]any{"title": s.Title, "artist": s.Artist, "bpm": s.BPM,
		"offset_ms": s.OffsetMS, "duration_ms": s.DurationMS, "charts": s.Charts})
	os.Stdout.Write(append(out, '\n'))
	log.Printf("%s: %.2f BPM, first beat %d ms, notes %v", s.ID, s.BPM, s.OffsetMS, s.Notes)
}

func decodeMono(data []byte) ([]float64, int, error) {
	d, err := mp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, 0, fmt.Errorf("decode mp3: %w", err)
	}
	factor := max(d.SampleRate()/bmRate, 1)
	rate := d.SampleRate() / factor
	if d.Length() > 0 && d.Length()/4/int64(d.SampleRate()) > bmMaxMS/1000 {
		return nil, 0, errors.New("audio is longer than 10 minutes")
	}
	var out []float64
	buf := make([]byte, 4*factor*2048)
	var acc float64
	n := 0
	for {
		k, err := io.ReadFull(d, buf)
		for i := 0; i+3 < k; i += 4 {
			l := int16(uint16(buf[i]) | uint16(buf[i+1])<<8)
			r := int16(uint16(buf[i+2]) | uint16(buf[i+3])<<8)
			acc += float64(int(l)+int(r)) / 65536
			if n++; n == factor {
				out = append(out, acc/float64(factor))
				acc, n = 0, 0
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		} else if err != nil {
			return nil, 0, fmt.Errorf("decode mp3: %w", err)
		}
		if len(out) > bmMaxMS/1000*rate {
			return nil, 0, errors.New("audio is longer than 10 minutes")
		}
	}
	return out, rate, nil
}

func analyse(x []float64, rate int) *onsets {
	frames := max((len(x)-bmWin)/bmHop+1, 0)
	o := &onsets{hopMS: 1000 * float64(bmHop) / float64(rate),
		flux: make([]float64, frames), low: make([]float64, frames), high: make([]float64, frames), rms: make([]float64, frames)}
	hann := make([]float64, bmWin)
	for i := range hann {
		hann[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/bmWin)
	}
	binHz := float64(rate) / bmWin
	lowTop, highBottom := int(200/binHz), int(2000/binHz)
	prev := make([]float64, bmWin/2)
	buf := make([]complex128, bmWin)
	for f := range frames {
		seg := x[f*bmHop : f*bmHop+bmWin]
		var sq float64
		for i, v := range seg {
			buf[i] = complex(v*hann[i], 0)
			sq += v * v
		}
		o.rms[f] = math.Sqrt(sq / bmWin)
		fft(buf)
		for k := 1; k < bmWin/2; k++ {
			m := math.Log1p(100 * cmplx.Abs(buf[k]))
			if d := m - prev[k]; d > 0 && f > 0 {
				o.flux[f] += d
				if k <= lowTop {
					o.low[f] += d
				} else if k >= highBottom {
					o.high[f] += d
				}
			}
			prev[k] = m
		}
	}
	win := int(250 / o.hopMS)
	for _, s := range [][]float64{o.flux, o.low, o.high} {
		detrend(s, win)
	}
	return o
}

// detrend subtracts a moving average so steady loud passages don't read as onsets, then scales to 0-1.
func detrend(s []float64, win int) {
	prefix := make([]float64, len(s)+1)
	for i, v := range s {
		prefix[i+1] = prefix[i] + v
	}
	for i := range s {
		lo, hi := max(i-win, 0), min(i+win+1, len(s))
		s[i] = max(s[i]-(prefix[hi]-prefix[lo])/float64(hi-lo), 0)
	}
	top := percentile(s, 0.995)
	for i := range s {
		s[i] = min(s[i]/max(top, 1e-9), 1)
	}
}

func fft(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		step := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := range size / 2 {
				u, v := a[start+k], a[start+k+size/2]*w
				a[start+k], a[start+k+size/2] = u+v, u-v
				w *= step
			}
		}
	}
}

// at is the strongest value of s within 25 ms of t.
func (o *onsets) at(s []float64, t float64) float64 {
	c := int(math.Round(t / o.hopMS))
	r := int(25 / o.hopMS)
	best := 0.0
	for i := max(c-r, 0); i <= c+r && i < len(s); i++ {
		best = max(best, s[i])
	}
	return best
}

// tempo finds the BPM by autocorrelating onset strength, then fits the BPM and
// first-beat phase that line beats up with the most onset strength.
func (o *onsets) tempo() (bpm, phaseMS float64) {
	coarse, best := 120.0, -1.0
	for b := 70.0; b <= 180; b += 0.5 {
		lag := 60000 / b / o.hopMS
		var s float64
		for i := range o.flux {
			if j := float64(i) + lag; int(j)+1 < len(o.flux) {
				fr := j - math.Floor(j)
				s += o.flux[i] * (o.flux[int(j)]*(1-fr) + o.flux[int(j)+1]*fr)
			}
		}
		// Prefer tempos near 120 so half and double time lose ties.
		s *= math.Exp(-0.5 * math.Pow(math.Log2(b/120)/0.6, 2))
		if s > best {
			coarse, best = b, s
		}
	}
	endMS := float64(len(o.flux)) * o.hopMS
	fit := func(b float64) (float64, float64) {
		period := 60000 / b
		bestPhase, bestScore := 0.0, -1.0
		for ph := 0.0; ph < period; ph += 5 {
			var s float64
			for t := ph; t < endMS; t += period {
				s += o.at(o.flux, t) + 0.5*o.at(o.low, t)
			}
			if s > bestScore {
				bestPhase, bestScore = ph, s
			}
		}
		return bestPhase, bestScore
	}
	bestScore := -1.0
	for b := coarse - 2; b <= coarse+2; b += 0.02 {
		if ph, s := fit(b); s > bestScore {
			bpm, phaseMS, bestScore = b, ph, s
		}
	}
	// Most produced music sits on a whole BPM; take it unless it fits clearly worse.
	if r := math.Round(bpm); r != bpm {
		if ph, s := fit(r); s >= 0.97*bestScore {
			bpm, phaseMS = r, ph
		}
	}
	return bpm, phaseMS
}

type slot struct {
	t        float64
	strength float64
	tone     float64 // high minus low onset strength
}

func (o *onsets) chart(spec beatSpec, beat, phase, durMS float64) []Note {
	step := beat / float64(spec.perBeat)
	var slots []slot
	for k := 0; ; k++ {
		t := phase + float64(k)*step
		if t > durMS-1500 {
			break
		}
		if t < bmLeadInMS {
			continue
		}
		w := 1.0
		if k%spec.perBeat != 0 {
			w = 0.7
		}
		slots = append(slots, slot{t, o.at(o.flux, t) * w, o.at(o.high, t) - o.at(o.low, t)})
	}
	if len(slots) == 0 {
		return []Note{}
	}
	strengths := make([]float64, len(slots))
	for i, s := range slots {
		strengths[i] = s.strength
	}
	floor := percentile(strengths, 1-spec.keep)

	// Strongest slots first, skipping any too close to one already taken.
	order := make([]int, len(slots))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return slots[order[a]].strength > slots[order[b]].strength })
	taken := make([]bool, len(slots))
	gap := int(math.Ceil(spec.minGapMS/step - 1e-9))
	for _, i := range order {
		if slots[i].strength < floor || slots[i].strength <= 0 {
			break
		}
		ok := true
		for j := max(i-gap+1, 0); j < min(i+gap, len(slots)); j++ {
			if taken[j] {
				ok = false
				break
			}
		}
		taken[i] = ok
	}
	var picked []slot
	for i, s := range slots {
		if taken[i] {
			picked = append(picked, s)
		}
	}
	chordFloor := percentile(strengths, 0.97)
	return assignLanes(picked, spec, beat, chordFloor, o)
}

// assignLanes puts the brighter half of the onsets on the top lanes (+ and -)
// and the rest on the bottom lanes (A and B), alternating sides within a pair.
func assignLanes(picked []slot, spec beatSpec, beat, chordFloor float64, o *onsets) []Note {
	notes := []Note{}
	tones := make([]float64, len(picked))
	for i, s := range picked {
		tones[i] = s.tone
	}
	split := percentile(tones, 0.5)
	var freeAt, holdEnd [4]float64
	last := [2]int{0, 2}
	lastT := [2]float64{-1e9, -1e9}
	var seen [2]int
	lastChord := -1
	medRMS := percentile(o.rms, 0.5)
	pick := func(pair int, t float64) int {
		a, b := pair*2, pair*2+1
		// Alternate sides, repeating a side once every few notes so it isn't a strict zigzag.
		seen[pair]++
		want := a + b - last[pair]
		if seen[pair]%5 == 0 && t-lastT[pair] >= 2*beat {
			want = last[pair]
		}
		if freeAt[want] > t {
			want = a + b - want
		}
		if freeAt[want] > t {
			return -1
		}
		return want
	}
	for i, s := range picked {
		pair := 1
		if s.tone > split {
			pair = 0
		}
		lane := pick(pair, s.t)
		if lane < 0 {
			pair = 1 - pair
			lane = pick(pair, s.t)
		}
		if lane < 0 {
			continue
		}
		lanes := []int{lane}
		if spec.chords && s.strength >= chordFloor && i-lastChord > 2 {
			if other := pick(1-pair, s.t); other >= 0 {
				lanes = append(lanes, other)
				lastChord = i
			}
		}
		next := math.Inf(1)
		if i+1 < len(picked) {
			next = picked[i+1].t
		}
		for _, l := range lanes {
			dur := 0
			// The + and - lanes never hold together, since pressing both pauses the game.
			if len(lanes) == 1 && i%3 == 2 && (l >= 2 || holdEnd[1-l] <= s.t) {
				dur = holdLength(spec, s.t, next, beat, o, medRMS)
			}
			if dur > 0 {
				notes = append(notes, Note{int(math.Round(s.t)), l, dur})
				holdEnd[l] = s.t + float64(dur)
				freeAt[l] = s.t + float64(dur) + beat
			} else {
				notes = append(notes, Note{int(math.Round(s.t)), l})
				freeAt[l] = s.t + min(2*beat, spec.minGapMS*2)
			}
			p := l / 2
			last[p], lastT[p] = l, s.t
		}
	}
	return notes
}

// holdLength turns a note into a hold over a gap with no other notes, as long
// as the music keeps sounding through it.
func holdLength(spec beatSpec, t, next, beat float64, o *onsets, medRMS float64) int {
	for k := len(spec.holdBeats) - 1; k >= 0; k-- {
		d := float64(spec.holdBeats[k]) * beat
		if t+d+beat/2 > next {
			continue
		}
		quiet := 0
		hops := 0
		for i := int(t / o.hopMS); float64(i)*o.hopMS < t+d && i < len(o.rms); i++ {
			hops++
			if o.rms[i] < 0.5*medRMS {
				quiet++
			}
		}
		if hops > 0 && quiet*4 < hops {
			return int(math.Round(d))
		}
	}
	return 0
}

func percentile(s []float64, p float64) float64 {
	if len(s) == 0 {
		return 0
	}
	c := append([]float64(nil), s...)
	sort.Float64s(c)
	return c[min(int(p*float64(len(c))), len(c)-1)]
}
