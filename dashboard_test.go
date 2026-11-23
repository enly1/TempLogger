package main

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func historyDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE readings (
		at INTEGER PRIMARY KEY, temperature REAL NOT NULL, humidity INTEGER NOT NULL,
		battery INTEGER NOT NULL, millivolts INTEGER NOT NULL, rssi INTEGER NOT NULL
	); CREATE TABLE deleted_readings (at INTEGER PRIMARY KEY)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertHistoryRows(t *testing.T, db *sql.DB, count int, at func(int) int64) {
	t.Helper()
	for i := 0; i < count; i++ {
		if _, err := db.Exec(`INSERT INTO readings
			(at, temperature, humidity, battery, millivolts, rssi) VALUES (?, ?, ?, 80, 2900, -60)`,
			at(i), 20+float64(i%7), 50+i%11); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHistoryBetweenRetainsMinimumSamplesUnderOneHour(t *testing.T) {
	db := historyDB(t)
	start := int64(1_700_000_000_000_000_000)
	insertHistoryRows(t, db, 120, func(i int) int64 {
		// Concentrate all readings inside one time bucket so the range sampler
		// would otherwise collapse them to only a few endpoints and extremes.
		return start + int64(i)*int64(time.Millisecond)
	})

	points, err := historyBetween(context.Background(), db, start, start+int64(30*time.Minute), 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) < 60 {
		t.Fatalf("got %d readings, want at least 60", len(points))
	}
	for i := 1; i < len(points); i++ {
		if points[i].At <= points[i-1].At {
			t.Fatalf("readings are not ordered at index %d", i)
		}
	}
}

func TestHistoryBetweenDoesNotInventSubHourSamples(t *testing.T) {
	db := historyDB(t)
	start := int64(1_700_000_000_000_000_000)
	insertHistoryRows(t, db, 20, func(i int) int64 {
		return start + int64(i)*int64(time.Millisecond)
	})

	points, err := historyBetween(context.Background(), db, start, start+int64(30*time.Minute), 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 || len(points) > 20 {
		t.Fatalf("got %d readings from 20 existing readings", len(points))
	}
}

func TestHistoryBetweenUsesRequestedBucketResolution(t *testing.T) {
	db := historyDB(t)
	start := int64(1_700_000_000_000_000_000)
	for i := 0; i < 3600; i++ {
		if _, err := db.Exec(`INSERT INTO readings
			(at, temperature, humidity, battery, millivolts, rssi) VALUES (?, 20, 50, 80, 2900, -60)`,
			start+int64(i)*int64(time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	from, to := start, start+int64(2*time.Hour)
	coarse, err := historyBetweenWithBuckets(context.Background(), db, from, to, 90*time.Second, 80)
	if err != nil {
		t.Fatal(err)
	}
	fine, err := historyBetweenWithBuckets(context.Background(), db, from, to, 90*time.Second, 400)
	if err != nil {
		t.Fatal(err)
	}
	if len(fine) <= len(coarse) {
		t.Fatalf("fine request returned %d points; coarse request returned %d", len(fine), len(coarse))
	}
}

func TestHistoryBetweenPreservesBatteryExtremes(t *testing.T) {
	db := historyDB(t)
	window := int64(2 * time.Hour)
	start := (int64(1_700_000_000_000_000_000) / window) * window
	battery := []int{80, 75, 4, 99, 78}
	for i, value := range battery {
		if _, err := db.Exec(`INSERT INTO readings
			(at, temperature, humidity, battery, millivolts, rssi) VALUES (?, 20, 50, ?, 2900, -60)`,
			start+int64(i)*int64(time.Second), value); err != nil {
			t.Fatal(err)
		}
	}

	points, err := historyBetweenWithBuckets(context.Background(), db, start, start+window, 90*time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int]bool{}
	for _, point := range points {
		found[point.Battery] = true
	}
	if !found[4] || !found[99] {
		t.Fatalf("battery extremes missing from sampled points: %+v", points)
	}
}

func TestDeleteHistoryBetweenDeletesRangeAndSuppressesCSVReimport(t *testing.T) {
	db := historyDB(t)
	start := int64(1_700_000_000_000_000_000)
	insertHistoryRows(t, db, 3, func(i int) int64 {
		return start + int64(i)*int64(time.Second)
	})

	deletedAt := start + int64(time.Second)
	deleted, err := deleteHistoryBetween(context.Background(), db, deletedAt, deletedAt+int64(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted %d readings, want 2", deleted)
	}

	if _, err := saveReading(context.Background(), db, reading{
		Time: time.Unix(0, deletedAt), Temp: 21, Humidity: 50, Battery: 80, Millivolts: 2900, RSSI: -60,
	}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRow("SELECT COUNT(*) FROM readings").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("got %d readings after deletion and reimport attempt, want 1", remaining)
	}
}
