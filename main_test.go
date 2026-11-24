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

func TestDecodeScanBroadcastFormats(t *testing.T) {
	atc := decodeBroadcastTemperature("0000181a-0000-1000-8000-00805f9b34fb", mustDecodeHex(t, "a4c138ea80e100e72e5a0bca08"))
	if atc == "" || !strings.Contains(atc, "ATC temperature 23.1°C") {
		t.Fatalf("ATC decode: %q", atc)
	}

	pvvx := decodeBroadcastTemperature("181a", []byte{0xa4, 0xc1, 0x38, 0xea, 0x80, 0xe1, 0x0e, 0x09, 0x88, 0x13, 0xb8, 0x0b, 90, 0, 0})
	if pvvx == "" || !strings.Contains(pvvx, "PVVX temperature 23.18°C") {
		t.Fatalf("PVVX decode: %q", pvvx)
	}

	bthome := decodeBroadcastTemperature("0000fcd2-0000-1000-8000-00805f9b34fb", []byte{0x40, 0x02, 0x09, 0x07, 0x03, 0x40, 0x1f})
	if bthome == "" || !strings.Contains(bthome, "temperature 18.01") || !strings.Contains(bthome, "humidity 80.00") {
		t.Fatalf("BTHome decode: %q", bthome)
	}

	miBeacon := decodeBroadcastTemperature("fe95", []byte{0x70, 0x58, 0x5b, 0x05, 0x01, 0x15, 0x52, 0xb5, 0x38, 0xc1, 0xa4, 0x28, 0x04, 0x10, 0x02, 0x6e, 0x00})
	if !strings.Contains(miBeacon, "LYWSD03MMC") || !strings.Contains(miBeacon, "temperature 11.0°C") {
		t.Fatalf("Xiaomi MiBeacon decode: %q", miBeacon)
	}
	noMiBeaconTemperature := decodeBroadcastTemperature("fe95", mustDecodeHex(t, "30585b05011552b538c1a4280100"))
	if !strings.Contains(noMiBeaconTemperature, "no temperature object") {
		t.Fatalf("expected Xiaomi capability packet to report no temperature: %q", noMiBeaconTemperature)
	}
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestParseRawScanAdvertisement(t *testing.T) {
	servicePayload := mustDecodeHex(t, "a4c138ea80e100e72e5a0bca08")
	packet := append([]byte{4, 0x09, 'A', 'T', 'C', 16, 0x16, 0x1a, 0x18}, servicePayload...)
	uuids, services, manufacturers := parseRawScanAdvertisement(packet)
	if len(uuids) != 0 || len(services) != 1 || len(manufacturers) != 0 {
		t.Fatalf("unexpected advertisement parse: uuids=%v services=%+v manufacturers=%+v", uuids, services, manufacturers)
	}
	if decoded := decodeBroadcastTemperature(services[0].UUID, services[0].Data); !strings.Contains(decoded, "23.1°C") {
		t.Fatalf("service data was not decoded: %q", decoded)
	}
}

func TestFormatScanAdvertisementUsesCommonFieldsAndEmbeddedMAC(t *testing.T) {
	serviceData := mustDecodeHex(t, "30585b05011552b538c1a4280100")
	_, line := formatScanAdvertisement(scanAdvertisement{
		Address:     "f76abb3e-7e05-0dc1-054c-75ac1914d90a",
		RSSI:        -75,
		ServiceData: []scanServiceData{{UUID: "0000fe95-0000-1000-8000-00805f9b34fb", Data: serviceData}},
	})
	for _, field := range []string{"Address: A4C138B55215", "Scanner ID: f76abb3e-7e05-0dc1-054c-75ac1914d90a", "Raw AD: unavailable", "Service UUIDs: none", "Manufacturer data: none", "Xiaomi MiBeacon"} {
		if !strings.Contains(line, field) {
			t.Fatalf("common scan output missing %q: %s", field, line)
		}
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
