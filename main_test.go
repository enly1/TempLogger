package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeATC(t *testing.T) {
	p, _ := hex.DecodeString("a4c138ea80e100e72e5a0bca08")
	r, ok := decodeATC(p, -35)
	if !ok || r.Temp != 23.1 || r.Humidity != 46 || r.Battery != 90 || r.Millivolts != 3018 {
		t.Fatalf("unexpected: %+v %v", r, ok)
	}
	p[5]++
	if _, ok = decodeATC(p, -35); ok {
		t.Fatal("accepted another sensor")
	}
	if _, ok = decodeATC(p[:4], 0); ok {
		t.Fatal("accepted truncated packet")
	}
	p, _ = hex.DecodeString("a4c138ea80e1ff9c2e5a0bca08")
	r, ok = decodeATC(p, 0)
	if !ok || r.Temp != -10 {
		t.Fatalf("negative temperature: %+v", r)
	}
}
func TestStoreRoundTripAndLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.csv")
	s := store{path: path}
	now := time.Now().UTC().Truncate(time.Second)
	if e := s.append(reading{Time: now, Temp: 23.1, Humidity: 46, Battery: 90, Millivolts: 3018, RSSI: -35}); e != nil {
		t.Fatal(e)
	}
	ps, e := s.load()
	if e != nil || len(ps) != 1 || ps[0].Temp != 23.1 {
		t.Fatalf("%+v %v", ps, e)
	}
	f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = f.WriteString("2026-11-16T16:53:41+00:00,BAFEC9E3-96FF-E442-6CF9-217C5BCB8F6A,LYWSD03MMC,ATC,23.1,46,90,3018,-35\n2026-11-16T16:53:41+00:00,other,other,ATC,99,0,0,0,0\n")
	f.Close()
	ps, e = s.load()
	if e != nil || len(ps) != 2 {
		t.Fatalf("legacy load: %+v %v", ps, e)
	}
}
func TestChartTimeBucketsAndGap(t *testing.T) {
	end := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	points := []reading{{Time: end.Add(-time.Hour), Temp: 20}, {Time: end, Temp: 25}}
	s := chart(points, 20, 5, 0, end.Add(-time.Hour), end)
	if strings.Count(s, "●") != 2 {
		t.Fatal(s)
	}
	if !strings.Contains(chart(nil, 20, 5, 0, end.Add(-time.Hour), end), "No readings") {
		t.Fatal("missing empty-state message")
	}
}
