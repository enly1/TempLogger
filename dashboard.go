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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	_ "github.com/mattn/go-sqlite3"
	esphome "github.com/richard87/esphome-apiclient"
	"github.com/richard87/esphome-apiclient/pb"
	"google.golang.org/protobuf/proto"
	"tinygo.org/x/bluetooth"
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

type dashboardSensorReading struct {
	At          int64    `json:"at"`
	DeviceID    string   `json:"deviceId"`
	Format      string   `json:"format"`
	Temperature *float64 `json:"temperature"`
	Humidity    *float64 `json:"humidity"`
	Battery     *float64 `json:"battery"`
	Millivolts  *float64 `json:"millivolts"`
	RSSI        int      `json:"rssi"`
	Segment     int64    `json:"segment"`
}

type dashboardSensor struct {
	DeviceID string `json:"deviceId"`
	Alias    string `json:"alias"`
	Format   string `json:"format"`
}

func sampleSensorReadings(points []dashboardSensorReading, from, to int64, buckets int64) []dashboardSensorReading {
	if len(points) < 2 || to <= from {
		return points
	}
	buckets = max(int64(1), buckets)
	width := max(int64(1), (to-from+1)/buckets)
	groups := make(map[string][]dashboardSensorReading)
	var ids []string
	for _, p := range points {
		if _, ok := groups[p.DeviceID]; !ok {
			ids = append(ids, p.DeviceID)
		}
		groups[p.DeviceID] = append(groups[p.DeviceID], p)
	}
	result := make([]dashboardSensorReading, 0)
	for _, id := range ids {
		series := groups[id]
		keep := map[int]bool{0: true, len(series) - 1: true}
		if to-from < int64(time.Hour) && len(series) >= 60 {
			for n := 1; n <= 60; n++ {
				keep[(n*len(series)+59)/60-1] = true
			}
		}
		for i := 1; i < len(series); i++ {
			if series[i].Segment != series[i-1].Segment {
				keep[i-1] = true
				keep[i] = true
			}
		}
		for start := 0; start < len(series); {
			bucket := (series[start].At - from) / width
			end := start + 1
			for end < len(series) && (series[end].At-from)/width == bucket {
				end++
			}
			keep[start] = true
			keep[end-1] = true
			for _, get := range []func(dashboardSensorReading) *float64{func(p dashboardSensorReading) *float64 { return p.Temperature }, func(p dashboardSensorReading) *float64 { return p.Humidity }, func(p dashboardSensorReading) *float64 { return p.Battery }, func(p dashboardSensorReading) *float64 { return p.Millivolts }} {
				lo, hi := -1, -1
				for i := start; i < end; i++ {
					v := get(series[i])
					if v == nil {
						continue
					}
					if lo < 0 || *v < *get(series[lo]) {
						lo = i
					}
					if hi < 0 || *v > *get(series[hi]) {
						hi = i
					}
				}
				if lo >= 0 {
					keep[lo] = true
					keep[hi] = true
				}
			}
			start = end
		}
		indexes := make([]int, 0, len(keep))
		for i := range keep {
			indexes = append(indexes, i)
		}
		sort.Ints(indexes)
		for _, i := range indexes {
			result = append(result, series[i])
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].At == result[j].At {
			return result[i].DeviceID < result[j].DeviceID
		}
		return result[i].At < result[j].At
	})
	return result
}

func saveSensorObservation(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, observation sensorObservation) (sql.Result, error) {
	return db.ExecContext(ctx, `INSERT OR IGNORE INTO sensor_readings
		(at, device_id, name, format, temperature, humidity, battery, millivolts, rssi)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (SELECT 1 FROM deleted_readings WHERE at = ?)`,
		observation.Time.UnixNano(), observation.DeviceID, "", observation.Format,
		observation.Temperature, observation.Humidity, observation.Battery, observation.Millivolts,
		observation.RSSI, observation.Time.UnixNano())
}

func getSensorAlias(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, deviceID string) string {
	var alias string
	if err := db.QueryRowContext(ctx, "SELECT alias FROM sensor_aliases WHERE device_id = ?", deviceID).Scan(&alias); err != nil {
		return ""
	}
	return alias
}

func describeSensorMetric(value *float64, format string, args ...any) string {
	if value == nil {
		return "n/a"
	}
	return fmt.Sprintf(format, append([]any{*value}, args...)...)
}

func logSensorObservation(observation sensorObservation, alias string) {
	packet := ""
	if observation.Counter != nil {
		packet = fmt.Sprintf(" packet #%d", *observation.Counter)
	}
	label := strings.TrimSpace(alias)
	if label == "" {
		label = observation.DeviceID
	}
	log.Printf("BLE sensor %s [%s, %s] RSSI %d dBm | temperature %s | humidity %s | battery %s | voltage %s%s",
		observation.DeviceID, label, observation.Format, observation.RSSI,
		describeSensorMetric(observation.Temperature, "%.2f°C"),
		describeSensorMetric(observation.Humidity, "%.1f%%"),
		describeSensorMetric(observation.Battery, "%.0f%%"),
		describeSensorMetric(observation.Millivolts, "%.0f mV"), packet)
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
		CREATE TABLE IF NOT EXISTS sensor_readings (
			at INTEGER NOT NULL, device_id TEXT NOT NULL, name TEXT NOT NULL, format TEXT NOT NULL,
			temperature REAL, humidity REAL, battery REAL, millivolts REAL, rssi INTEGER NOT NULL,
			PRIMARY KEY(at, device_id, format)
		);
		CREATE INDEX IF NOT EXISTS sensor_readings_at ON sensor_readings(at);
		CREATE TABLE IF NOT EXISTS sensor_aliases (
			device_id TEXT PRIMARY KEY, alias TEXT NOT NULL
		);
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
	// Bring the existing single-sensor history into the normalized chart store
	// so it remains visible alongside readings collected after the upgrade.
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO sensor_readings
		(at, device_id, name, format, temperature, humidity, battery, millivolts, rssi)
		SELECT r.at, ?, '', 'ATC', r.temperature, r.humidity, r.battery, r.millivolts, r.rssi
		FROM readings r WHERE NOT EXISTS (SELECT 1 FROM deleted_readings d WHERE d.at = r.at)`, "A4C138EA80E1"); err != nil {
		return fmt.Errorf("migrate existing chart readings: %w", err)
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
		SELECT at FROM readings WHERE at BETWEEN ? AND ? UNION SELECT at FROM sensor_readings WHERE at BETWEEN ? AND ?`, from, to, from, to); err != nil {
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
	sensorResult, err := tx.ExecContext(ctx, "DELETE FROM sensor_readings WHERE at BETWEEN ? AND ?", from, to)
	if err != nil {
		return 0, err
	}
	sensorDeleted, err := sensorResult.RowsAffected()
	if err != nil {
		return 0, err
	}
	deleted += sensorDeleted
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
		var latest any
		var sensor dashboardSensorReading
		err := db.QueryRowContext(r.Context(), `SELECT at, device_id, format, temperature, humidity, battery, millivolts, rssi
			FROM sensor_readings ORDER BY at DESC LIMIT 1`).
			Scan(&sensor.At, &sensor.DeviceID, &sensor.Format, &sensor.Temperature, &sensor.Humidity, &sensor.Battery, &sensor.Millivolts, &sensor.RSSI)
		if err == nil {
			sensor.At /= int64(time.Millisecond)
			latest = sensor
		} else if errors.Is(err, sql.ErrNoRows) {
			var p dashboardReading
			err = db.QueryRowContext(r.Context(), "SELECT "+readingColumns+" FROM readings ORDER BY at DESC LIMIT 1").
				Scan(&p.At, &p.Temperature, &p.Humidity, &p.Battery, &p.Millivolts, &p.RSSI)
			if err == nil {
				p.At /= int64(time.Millisecond)
				latest = p
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Print(err)
			http.Error(w, "Could not read history", http.StatusInternalServerError)
			return
		}
		var first sql.NullInt64
		if err := db.QueryRowContext(r.Context(), "SELECT MIN(at) FROM (SELECT at FROM readings UNION ALL SELECT at FROM sensor_readings)").Scan(&first); err != nil {
			log.Print(err)
			http.Error(w, "Could not read history", http.StatusInternalServerError)
			return
		}
		rows, err := db.QueryContext(r.Context(), `WITH ranked AS (
			SELECT at, device_id, format, temperature, humidity, battery, millivolts, rssi,
			ROW_NUMBER() OVER (PARTITION BY device_id ORDER BY at DESC) AS row_number
			FROM sensor_readings
		) SELECT at, device_id, format, temperature, humidity, battery, millivolts, rssi
		FROM ranked WHERE row_number = 1 ORDER BY device_id`)
		if err != nil {
			log.Print(err)
			http.Error(w, "Could not read current sensor values", http.StatusInternalServerError)
			return
		}
		latestSensors := make([]dashboardSensorReading, 0)
		for rows.Next() {
			var sensor dashboardSensorReading
			if err := rows.Scan(&sensor.At, &sensor.DeviceID, &sensor.Format, &sensor.Temperature, &sensor.Humidity, &sensor.Battery, &sensor.Millivolts, &sensor.RSSI); err != nil {
				rows.Close()
				http.Error(w, "Could not read current sensor values", http.StatusInternalServerError)
				return
			}
			sensor.At /= int64(time.Millisecond)
			latestSensors = append(latestSensors, sensor)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			log.Print(err)
			http.Error(w, "Could not read current sensor values", http.StatusInternalServerError)
			return
		}
		rows.Close()
		sendJSON(w, map[string]any{"latest": latest, "latestSensors": latestSensors, "first": first.Int64 / int64(time.Millisecond),
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
	mux.HandleFunc("GET /api/sensors", func(w http.ResponseWriter, r *http.Request) {
		rows, err := db.QueryContext(r.Context(), `SELECT readings.device_id, COALESCE(aliases.alias, ''),
			GROUP_CONCAT(DISTINCT readings.format) FROM sensor_readings AS readings
			LEFT JOIN sensor_aliases AS aliases ON aliases.device_id = readings.device_id
			GROUP BY readings.device_id, aliases.alias ORDER BY readings.device_id`)
		if err != nil {
			http.Error(w, "Could not read sensors", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		sensors := make([]dashboardSensor, 0)
		for rows.Next() {
			var sensor dashboardSensor
			if err := rows.Scan(&sensor.DeviceID, &sensor.Alias, &sensor.Format); err != nil {
				http.Error(w, "Could not read sensors", http.StatusInternalServerError)
				return
			}
			sensors = append(sensors, sensor)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "Could not read sensors", http.StatusInternalServerError)
			return
		}
		sendJSON(w, sensors)
	})
	mux.HandleFunc("PUT /api/sensors/{deviceId}/alias", func(w http.ResponseWriter, r *http.Request) {
		deviceID := strings.TrimSpace(r.PathValue("deviceId"))
		if deviceID == "" || len(deviceID) > 128 {
			http.Error(w, "Expected a valid sensor ID", http.StatusBadRequest)
			return
		}
		var request struct {
			Alias string `json:"alias"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "Expected a JSON alias", http.StatusBadRequest)
			return
		}
		request.Alias = strings.TrimSpace(request.Alias)
		if len(request.Alias) > 120 {
			http.Error(w, "Alias must be 120 bytes or fewer", http.StatusBadRequest)
			return
		}
		var err error
		if request.Alias == "" {
			_, err = db.ExecContext(r.Context(), "DELETE FROM sensor_aliases WHERE device_id = ?", deviceID)
		} else {
			_, err = db.ExecContext(r.Context(), `INSERT INTO sensor_aliases(device_id, alias) VALUES(?, ?)
				ON CONFLICT(device_id) DO UPDATE SET alias = excluded.alias`, deviceID, request.Alias)
		}
		if err != nil {
			log.Print(err)
			http.Error(w, "Could not save sensor alias", http.StatusInternalServerError)
			return
		}
		sendJSON(w, map[string]string{"deviceId": deviceID, "alias": request.Alias})
	})
	mux.HandleFunc("GET /api/sensor-readings", func(w http.ResponseWriter, r *http.Request) {
		from, fromErr := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
		to, toErr := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64)
		if fromErr != nil || toErr != nil || from < 0 || to <= from || to > 9223372036853 {
			http.Error(w, "Expected a valid from/to range in milliseconds", http.StatusBadRequest)
			return
		}
		rows, err := db.QueryContext(r.Context(), `WITH ordered AS (
			SELECT *, at - LAG(at) OVER (PARTITION BY device_id ORDER BY at) AS elapsed
			FROM sensor_readings WHERE at BETWEEN ? AND ?
		), segmented AS (
			SELECT *, SUM(CASE WHEN elapsed > ? THEN 1 ELSE 0 END) OVER (PARTITION BY device_id ORDER BY at) AS segment FROM ordered
		) SELECT at, device_id, format, temperature, humidity, battery, millivolts, rssi, segment
		FROM segmented ORDER BY at`, from*int64(time.Millisecond), (to+1)*int64(time.Millisecond)-1, max(90*time.Second, interval*3).Nanoseconds())
		if err != nil {
			http.Error(w, "Could not read sensor history", http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		readings := make([]dashboardSensorReading, 0)
		for rows.Next() {
			var p dashboardSensorReading
			if err := rows.Scan(&p.At, &p.DeviceID, &p.Format, &p.Temperature, &p.Humidity, &p.Battery, &p.Millivolts, &p.RSSI, &p.Segment); err != nil {
				http.Error(w, "Could not read sensor history", http.StatusInternalServerError)
				return
			}
			p.At /= int64(time.Millisecond)
			readings = append(readings, p)
		}
		if err := rows.Err(); err != nil {
			http.Error(w, "Could not read sensor history", http.StatusInternalServerError)
			return
		}
		buckets := int64(750)
		if raw := r.URL.Query().Get("buckets"); raw != "" {
			if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 && n <= 5000 {
				buckets = n
			}
		}
		readings = sampleSensorReadings(readings, from, to, buckets)
		sendJSON(w, readings)
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

func runDashboardCollector(ctx context.Context, db *sql.DB, _ time.Duration, status *atomic.Value, proxyHost, proxyKey string, localBLE bool) {
	events := make(chan event, 1)
	if localBLE {
		go collectLocalSensors(ctx, db, events)
	} else {
		go collectProxySensors(ctx, proxyHost, proxyKey, db, events)
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

func collectLocalSensors(ctx context.Context, db *sql.DB, events chan<- event) {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		events <- event{err: fmt.Errorf("Bluetooth: %w", err)}
		return
	}
	seen := make(map[string]uint8)
	scanDone := make(chan error, 1)
	go func() {
		scanDone <- adapter.Scan(func(_ *bluetooth.Adapter, result bluetooth.ScanResult) {
			adv := scanAdvertisement{Address: result.Address.String(), RSSI: int(result.RSSI), Raw: append([]byte(nil), result.Bytes()...)}
			for _, uuid := range result.ServiceUUIDs() {
				adv.ServiceUUIDs = append(adv.ServiceUUIDs, uuid.String())
			}
			for _, service := range result.ServiceData() {
				adv.ServiceData = append(adv.ServiceData, scanServiceData{UUID: service.UUID.String(), Data: append([]byte(nil), service.Data...)})
			}
			for _, manufacturer := range result.ManufacturerData() {
				adv.ManufacturerData = append(adv.ManufacturerData, scanManufacturerData{ID: fmt.Sprintf("%04x", manufacturer.CompanyID), Data: append([]byte(nil), manufacturer.Data...)})
			}
			for _, observation := range decodeSensorObservations(adv) {
				fingerprint := observation.DeviceID + "/" + observation.Format
				if observation.Counter != nil {
					if counter, ok := seen[fingerprint]; ok && counter == *observation.Counter {
						continue
					}
					seen[fingerprint] = *observation.Counter
				}
				logSensorObservation(observation, getSensorAlias(ctx, db, observation.DeviceID))
				if _, err := saveSensorObservation(ctx, db, observation); err != nil {
					select {
					case events <- event{err: fmt.Errorf("saving sensor reading: %w", err)}:
					default:
					}
					continue
				}
				select {
				case events <- event{}:
				default:
				}
			}
		})
	}()
	select {
	case <-ctx.Done():
		_ = adapter.StopScan()
		<-scanDone
	case err := <-scanDone:
		if err == nil {
			err = errors.New("local Bluetooth scan stopped unexpectedly")
		}
		select {
		case events <- event{err: fmt.Errorf("local Bluetooth scan: %w", err)}:
		default:
		}
	}
}

func collectProxySensors(ctx context.Context, host, key string, db *sql.DB, events chan<- event) {
	address := proxyAddress(host)
	seen := make(map[string]uint8)
	for ctx.Err() == nil {
		opts := []esphome.Option{esphome.WithClientInfo("TempLogger dashboard sensor collector")}
		if key != "" {
			opts = append(opts, esphome.WithEncryptionKey(key))
		}
		client, err := esphome.DialWithContext(ctx, address, 5*time.Second, opts...)
		if err != nil {
			if !waitProxyReconnect(ctx, address) {
				return
			}
			continue
		}
		requestPassiveProxyScan(client)
		var lastValid atomic.Int64
		lastValid.Store(time.Now().UnixNano())
		handler := func(msg proto.Message) {
			var advertisements []scanAdvertisement
			switch batch := msg.(type) {
			case *pb.BluetoothLERawAdvertisementsResponse:
				for _, adv := range batch.Advertisements {
					uuids, services, manufacturers := parseRawScanAdvertisement(adv.Data)
					advertisements = append(advertisements, scanAdvertisement{Address: fmt.Sprintf("%012X", adv.Address), RSSI: int(adv.Rssi), Raw: adv.Data, ServiceUUIDs: uuids, ServiceData: services, ManufacturerData: manufacturers})
				}
			case *pb.BluetoothLEAdvertisementResponse:
				adv := scanAdvertisement{Address: fmt.Sprintf("%012X", batch.Address), RSSI: int(batch.Rssi), ServiceUUIDs: batch.ServiceUuids}
				for _, service := range batch.ServiceData {
					adv.ServiceData = append(adv.ServiceData, scanServiceData{UUID: service.Uuid, Data: service.Data})
				}
				advertisements = append(advertisements, adv)
			}
			for _, adv := range advertisements {
				for _, observation := range decodeSensorObservations(adv) {
					lastValid.Store(time.Now().UnixNano())
					fingerprint := observation.DeviceID + "/" + observation.Format
					if observation.Counter != nil {
						if counter, exists := seen[fingerprint]; exists && counter == *observation.Counter {
							continue
						}
						seen[fingerprint] = *observation.Counter
					}
					logSensorObservation(observation, getSensorAlias(ctx, db, observation.DeviceID))
					if _, err := saveSensorObservation(ctx, db, observation); err != nil {
						select {
						case events <- event{err: fmt.Errorf("saving sensor reading: %w", err)}:
						default:
						}
						continue
					}
					select {
					case events <- event{}:
					default:
					}
				}
			}
		}
		unsubscribe, err := subscribeProxyAdvertisements(client, handler)
		if err != nil {
			_ = client.Close()
			if !waitProxyReconnect(ctx, address) {
				return
			}
			continue
		}
		idleTicker := time.NewTicker(30 * time.Second)
		connected := true
		for connected {
			select {
			case <-ctx.Done():
				idleTicker.Stop()
				unsubscribe()
				_ = client.Close()
				return
			case <-client.Done():
				connected = false
			case <-idleTicker.C:
				if time.Since(time.Unix(0, lastValid.Load())) >= 30*time.Second {
					log.Printf("No valid proxy sensor packet received for 30s; refreshing dashboard BLE subscription")
					unsubscribe()
					lastValid.Store(time.Now().UnixNano())
					unsubscribe, err = subscribeProxyAdvertisements(client, handler)
					if err != nil {
						log.Printf("Refreshing dashboard BLE subscription via %s failed: %v", address, err)
						connected = false
					}
				}
			}
		}
		idleTicker.Stop()
		unsubscribe()
		_ = client.Close()
		if !waitProxyReconnect(ctx, address) {
			return
		}
	}
}
