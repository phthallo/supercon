package main

import (
	"math"
	"math/rand"
	"testing"
)

func TestChartNotesSitOnRedraws(t *testing.T) {
	const hop, durMS = 10.0, 120000.0
	n := int(durMS / hop)
	rng := rand.New(rand.NewSource(1))
	o := &onsets{hopMS: hop, flux: make([]float64, n), low: make([]float64, n), high: make([]float64, n), rms: make([]float64, n)}
	for i := range n {
		o.flux[i], o.low[i], o.high[i], o.rms[i] = rng.Float64(), rng.Float64(), rng.Float64(), 1
	}
	for _, bpm := range []float64{130, 120, 174, 80} {
		beat := 60000 / bpm
		step := beat * float64(redrawBeats(beat))
		if step < minRedrawMS || step-beat >= minRedrawMS {
			t.Fatalf("%v BPM: redraw every %.0f ms", bpm, step)
		}
		const phase = 90.0
		onRedraw := func(ms int) bool {
			k := math.Round((float64(ms) - phase) / step)
			return math.Abs(phase+k*step-float64(ms)) <= 0.5
		}
		for _, spec := range beatSpecs {
			notes := o.chart(spec, beat, phase, durMS)
			if len(notes) == 0 {
				t.Fatalf("%v BPM %s: no notes", bpm, spec.diff)
			}
			for _, note := range notes {
				if !onRedraw(note[0]) || (len(note) > 2 && !onRedraw(note[0]+note[2])) {
					t.Fatalf("%v BPM %s: note %v is between redraws every %.1f ms", bpm, spec.diff, note, step)
				}
			}
		}
	}
}
