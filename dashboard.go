package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed web/*
var dashboardFiles embed.FS

const readingColumns = "at, temperature, humidity, battery, millivolts, rssi"
const insertReading = `INSERT INTO readings (` + readingColumns + `)
	SELECT ?, ?, ?, ?, ?, ?
	WHERE NOT EXISTS (SELECT 1 FROM deleted_readings WHERE at = ?)
	ON CONFLICT(at) DO NOTHING`

type dashboardReading struct {
	At          int64   `json:"at"`
	Temperature float64 `json:"temperature"`
	Humidity    int     `json:"humidity"`
	Battery     int     `json:"battery"`
	Millivolts  int     `json:"millivolts"`
	RSSI        int     `json:"rssi"`
	Segment     int64   `json:"segment"`
}

func saveReading(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, r reading) (sql.Result, error) {
	at := r.Time.UnixNano()
	return db.ExecContext(ctx, insertReading, at, r.Temp, r.Humidity, r.Battery, r.Millivolts, r.RSSI, at)
}

func openHistory(ctx context.Context, dbPath, csvPath string) (*sql.DB, error) {
	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	csvPath, err = filepath.Abs(csvPath)
	if err != nil {
		return nil, err
	}
	if dbPath == csvPath {
		return nil, errors.New("use different paths for SQLite and CSV")
	}
	file, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := initHistory(ctx, db, csvPath); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func initHistory(ctx context.Context, db *sql.DB, csvPath string) error {
	if _, err := db.ExecContext(ctx, `
		PRAGMA busy_timeout = 5000;
		PRAGMA journal_mode = WAL;
		CREATE TABLE IF NOT EXISTS readings (
			at INTEGER PRIMARY KEY, temperature REAL NOT NULL, humidity INTEGER NOT NULL,
			battery INTEGER NOT NULL, millivolts INTEGER NOT NULL, rssi INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS deleted_readings (at INTEGER PRIMARY KEY);
	`); err != nil {
		return err
	}
	csv := &store{path: csvPath}
	points, err := csv.load()
	if err != nil {
		return fmt.Errorf("import %s: %w", csvPath, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var imported int64
	for _, point := range points {
		result, err := saveReading(ctx, tx, point)
		if err != nil {
			return err
		}
		count, _ := result.RowsAffected()
		imported += count
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if imported > 0 {
		log.Printf("Imported %d readings from %s", imported, csvPath)
	}
	return nil
}

func historyBetween(ctx context.Context, db *sql.DB, from, to int64, gap time.Duration) ([]dashboardReading, error) {
	return historyBetweenWithBuckets(ctx, db, from, to, gap, 750)
}

func historyBetweenWithBuckets(ctx context.Context, db *sql.DB, from, to int64, gap time.Duration, targetBuckets int64) ([]dashboardReading, error) {
	// Keep long ranges small. Each populated time bucket retains actual readings
	// at its endpoints and at temperature and humidity extremes. Short ranges
	// also retain at least 60 evenly distributed actual readings when available.
	targetBuckets = max(int64(1), targetBuckets)
	bucket := max(int64(1), (to-from)/targetBuckets)
	minimumSamples := 0
	if to-from < int64(time.Hour) {
		minimumSamples = 60
	}
	rows, err := db.QueryContext(ctx, `
		WITH RECURSIVE chronological AS (
			SELECT *, at - LAG(at) OVER (ORDER BY at) AS elapsed,
				ROW_NUMBER() OVER (ORDER BY at) AS position, COUNT(*) OVER () AS total_rows
			FROM readings WHERE at BETWEEN ? AND ?
		), bins AS (
			SELECT *, at / ? AS bucket,
				SUM(CASE WHEN elapsed > ? THEN 1 ELSE 0 END) OVER (ORDER BY at) AS segment
			FROM chronological
		), bounds AS (
			SELECT *,
				MIN(at) OVER bin AS first, MAX(at) OVER bin AS last,
				MIN(temperature) OVER bin AS low_temp, MAX(temperature) OVER bin AS high_temp,
				MIN(humidity) OVER bin AS low_humidity, MAX(humidity) OVER bin AS high_humidity,
				MIN(battery) OVER bin AS low_battery, MAX(battery) OVER bin AS high_battery
			FROM bins WINDOW bin AS (PARTITION BY bucket)
		), targets(n) AS (
			SELECT 1 WHERE ? > 0
			UNION ALL SELECT n + 1 FROM targets WHERE n < ?
		), selected AS (
			SELECT first AS at FROM bounds UNION SELECT last FROM bounds
			UNION SELECT MIN(at) FROM bounds WHERE temperature = low_temp GROUP BY bucket
			UNION SELECT MIN(at) FROM bounds WHERE temperature = high_temp GROUP BY bucket
			UNION SELECT MIN(at) FROM bounds WHERE humidity = low_humidity GROUP BY bucket
			UNION SELECT MIN(at) FROM bounds WHERE humidity = high_humidity GROUP BY bucket
			UNION SELECT MIN(at) FROM bounds WHERE battery = low_battery GROUP BY bucket
			UNION SELECT MIN(at) FROM bounds WHERE battery = high_battery GROUP BY bucket
			UNION SELECT at FROM chronological JOIN targets
				ON position = (targets.n * total_rows + 59) / 60
				WHERE total_rows >= 60
		)
		SELECT bounds.at, temperature, humidity, battery, millivolts, rssi, segment
		FROM selected JOIN bounds ON bounds.at = selected.at ORDER BY bounds.at
	`, from, to, bucket, gap.Nanoseconds(), minimumSamples, minimumSamples)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make([]dashboardReading, 0)
	for rows.Next() {
		var p dashboardReading
		if err := rows.Scan(&p.At, &p.Temperature, &p.Humidity, &p.Battery, &p.Millivolts, &p.RSSI, &p.Segment); err != nil {
			return nil, err
		}
		p.At /= int64(time.Millisecond)
		points = append(points, p)
	}
	return points, rows.Err()
}

func deleteHistoryBetween(ctx context.Context, db *sql.DB, from, to int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO deleted_readings (at)
		SELECT at FROM readings WHERE at BETWEEN ? AND ?`, from, to); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM readings WHERE at BETWEEN ? AND ?", from, to)
	if err != nil {
		return 0, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func dashboardHandler(db *sql.DB, interval time.Duration, status *atomic.Value) http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(dashboardFiles, "web")
	mux.Handle("GET /", http.FileServer(http.FS(assets)))
	sendJSON := func(w http.ResponseWriter, data any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(data)
	}
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		var p dashboardReading
		var latest *dashboardReading
		err := db.QueryRowContext(r.Context(), "SELECT "+readingColumns+" FROM readings ORDER BY at DESC LIMIT 1").
			Scan(&p.At, &p.Temperature, &p.Humidity, &p.Battery, &p.Millivolts, &p.RSSI)
		if err == nil {
			p.At /= int64(time.Millisecond)
			latest = &p
		} else if !errors.Is(err, sql.ErrNoRows) {
			log.Print(err)
			http.Error(w, "Could not read history", http.StatusInternalServerError)
			return
		}
		var first sql.NullInt64
		if err := db.QueryRowContext(r.Context(), "SELECT MIN(at) FROM readings").Scan(&first); err != nil {
			log.Print(err)
			http.Error(w, "Could not read history", http.StatusInternalServerError)
			return
		}
		sendJSON(w, map[string]any{"latest": latest, "first": first.Int64 / int64(time.Millisecond),
			"interval": interval.Milliseconds(), "state": status.Load(), "now": time.Now().UnixMilli()})
	})
	mux.HandleFunc("GET /api/readings", func(w http.ResponseWriter, r *http.Request) {
		from, fromErr := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, toErr := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		if fromErr != nil || toErr != nil || from < 0 || to <= from || to > 9223372036853 {
			http.Error(w, "Expected a valid from/to range in milliseconds", http.StatusBadRequest)
			return
		}
		targetBuckets := int64(750)
		if rawBuckets := r.URL.Query().Get("buckets"); rawBuckets != "" {
			parsed, err := strconv.ParseInt(rawBuckets, 10, 64)
			if err != nil || parsed < 1 || parsed > 5000 {
				http.Error(w, "Expected buckets between 1 and 5000", http.StatusBadRequest)
				return
			}
			targetBuckets = parsed
		}
		points, err := historyBetweenWithBuckets(r.Context(), db, from*int64(time.Millisecond), (to+1)*int64(time.Millisecond)-1, max(90*time.Second, interval*3), targetBuckets)
		if err != nil {
			log.Print(err)
			http.Error(w, "Could not read history", http.StatusInternalServerError)
			return
		}
		sendJSON(w, points)
	})
	mux.HandleFunc("DELETE /api/readings", func(w http.ResponseWriter, r *http.Request) {
		from, fromErr := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, toErr := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		if fromErr != nil || toErr != nil || from < 0 || to <= from || to > 9223372036853 {
			http.Error(w, "Expected a valid from/to range in milliseconds", http.StatusBadRequest)
			return
		}
		deleted, err := deleteHistoryBetween(r.Context(), db,
			from*int64(time.Millisecond), (to+1)*int64(time.Millisecond)-1)
		if err != nil {
			log.Print(err)
			http.Error(w, "Could not delete history", http.StatusInternalServerError)
			return
		}
		sendJSON(w, map[string]int64{"deleted": deleted})
	})
	return mux
}

func runDashboard(dbPath, csvPath, bind string, port int, interval time.Duration, viewOnly bool, proxyHost, proxyKey string, localBLE bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	addr := net.JoinHostPort(bind, fmt.Sprint(port))
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	defer listener.Close()
	db, err := openHistory(ctx, dbPath, csvPath)
	if err != nil {
		return fmt.Errorf("open SQLite history: %w", err)
	}
	defer db.Close()
	var status atomic.Value
	status.Store("History only")
	if !viewOnly {
		if localBLE {
			status.Store("Scanning for sensor")
		} else {
			status.Store("Connecting to Bluetooth proxy")
		}
		go runDashboardCollector(ctx, db, interval, &status, proxyHost, proxyKey, localBLE)
	}
	server := &http.Server{Handler: dashboardHandler(db, interval, &status),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
	}()
	url := "http://" + addr
	if isatty.IsTerminal(os.Stderr.Fd()) && os.Getenv("TERM") != "dumb" {
		// OSC 8 makes the visible URL a link in terminals that support it.
		url = "\x1b]8;;" + url + "\x1b\\" + url + "\x1b]8;;\x1b\\"
	}
	log.Printf("Dashboard: %s  ·  SQLite: %s", url, dbPath)
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runDashboardCollector(ctx context.Context, db *sql.DB, interval time.Duration, status *atomic.Value, proxyHost, proxyKey string, localBLE bool) {
	events := make(chan event, 1)
	lastSaved := time.Time{}
	save := func(r reading) error {
		if interval > 0 && !lastSaved.IsZero() && r.Time.Sub(lastSaved) < interval {
			return nil
		}
		_, err := saveReading(ctx, db, r)
		if err == nil {
			lastSaved = r.Time
		}
		return err
	}
	if localBLE {
		go collectLocal(ctx, 0, save, events, false)
	} else {
		go collectProxy(ctx, proxyHost, proxyKey, 0, save, events, false, true)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-events:
			if result.err != nil {
				log.Print(result.err)
				status.Store("Collection stopped · " + result.err.Error())
				return
			}
			status.Store("Collecting")
		}
	}
}
