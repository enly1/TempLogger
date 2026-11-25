package main

import (
	"context"
	"encoding/binary"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	esphome "github.com/richard87/esphome-apiclient"
	"github.com/richard87/esphome-apiclient/pb"
	"google.golang.org/protobuf/proto"
	"tinygo.org/x/bluetooth"
)

const sensorMAC = "A4:C1:38:EA:80:E1"

var header = []string{"timestamp_utc", "device_id", "name", "format", "temperature_c", "humidity_percent", "battery_percent", "battery_mv", "rssi_dbm"}

type reading struct {
	Time                                time.Time
	Temp                                float64
	Humidity, Battery, Millivolts, RSSI int
	Counter                             uint8
}

type sensorObservation struct {
	Time        time.Time
	DeviceID    string
	Format      string
	Temperature *float64
	Humidity    *float64
	Battery     *float64
	Millivolts  *float64
	RSSI        int
	Counter     *uint8
}

func floatPtr(value float64) *float64 { return &value }

func normalizeSensorDeviceID(deviceID string) string {
	compact := strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(deviceID))
	if len(compact) != 12 {
		return deviceID
	}
	for _, char := range compact {
		if (char < '0' || char > '9') && (char < 'A' || char > 'F') && (char < 'a' || char > 'f') {
			return deviceID
		}
	}
	return strings.ToUpper(compact)
}

func decodeATC(p []byte, rssi int) (reading, bool) {
	if len(p) != 13 || fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", p[0], p[1], p[2], p[3], p[4], p[5]) != sensorMAC {
		return reading{}, false
	}
	return reading{Time: time.Now().UTC(), Temp: float64(int16(binary.BigEndian.Uint16(p[6:8]))) / 10, Humidity: int(p[8]), Battery: int(p[9]), Millivolts: int(binary.BigEndian.Uint16(p[10:12])), RSSI: rssi, Counter: p[12]}, true
}

// decodeProxyAdvertisement extracts 0x181A service data from an ESPHome proxy.
func decodeProxyAdvertisement(data []byte, rssi int) (reading, bool) {
	var serviceData []byte
	for len(data) > 0 {
		fieldLen := int(data[0])
		if fieldLen == 0 {
			break
		}
		if fieldLen+1 > len(data) {
			break
		}
		fieldType := data[1]
		field := data[2 : fieldLen+1]
		switch fieldType {
		case 0x16: // 16-bit UUID service data
			if len(field) >= 2 && field[0] == 0x1a && field[1] == 0x18 {
				serviceData = field[2:]
			}
		}
		data = data[fieldLen+1:]
	}
	return decodeATC(serviceData, rssi)
}

func formatPacket(r reading) string {
	return fmt.Sprintf("ATC_EA80E1  %.1f°C  %d%% humidity  battery %d%% (%.3f V) packet #%d",
		r.Temp, r.Humidity, r.Battery, float64(r.Millivolts)/1000, r.Counter)
}

type store struct {
	mu   sync.Mutex
	path string
}

func (s *store) append(r reading) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if info.Size() > 0 {
		rd := csv.NewReader(f)
		h, e := rd.Read()
		if e != nil {
			return fmt.Errorf("read CSV header: %w", e)
		}
		if strings.Join(h, ",") != strings.Join(header, ",") {
			return errors.New("CSV header does not match expected format")
		}
	}
	w := csv.NewWriter(f)
	if info.Size() == 0 {
		if e = w.Write(header); e != nil {
			return e
		}
	}
	e = w.Write([]string{r.Time.UTC().Format(time.RFC3339Nano), sensorMAC, "", "ATC", strconv.FormatFloat(r.Temp, 'f', 1, 64), strconv.Itoa(r.Humidity), strconv.Itoa(r.Battery), strconv.Itoa(r.Millivolts), strconv.Itoa(r.RSSI)})
	if e != nil {
		return e
	}
	w.Flush()
	if e = w.Error(); e != nil {
		return e
	}
	return f.Sync()
}
func (s *store) load() ([]reading, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, e := os.Open(s.path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	rd := csv.NewReader(f)
	h, e := rd.Read()
	if e == io.EOF {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if strings.Join(h, ",") != strings.Join(header, ",") {
		return nil, errors.New("CSV header does not match expected format")
	}
	var all []reading
	for {
		row, e := rd.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("read CSV: %w", e)
		}
		if !strings.EqualFold(row[1], sensorMAC) && !strings.EqualFold(row[1], "BAFEC9E3-96FF-E442-6CF9-217C5BCB8F6A") {
			continue
		}
		t, e := time.Parse(time.RFC3339Nano, row[0])
		if e != nil {
			return nil, fmt.Errorf("invalid timestamp %q", row[0])
		}
		v, e := strconv.ParseFloat(row[4], 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("invalid temperature %q", row[4])
		}
		hum, _ := strconv.Atoi(row[5])
		bat, _ := strconv.Atoi(row[6])
		mv, _ := strconv.Atoi(row[7])
		rssi, _ := strconv.Atoi(row[8])
		all = append(all, reading{Time: t, Temp: v, Humidity: hum, Battery: bat, Millivolts: mv, RSSI: rssi})
	}
	return all, nil
}

type event struct {
	reading *reading
	err     error
}

type packet struct {
	reading reading
}

// packetCounterDeduper drops repeated advertisements for the same sensor
// measurement. The counter is an 8-bit measurement sequence and may wrap.
type packetCounterDeduper struct {
	last    uint8
	seenAny bool
}

func (d *packetCounterDeduper) first(r reading) bool {
	if d.seenAny && r.Counter == d.last {
		return false
	}
	d.last = r.Counter
	d.seenAny = true
	return true
}

type packetSchedule struct {
	anchor          time.Time
	interval        time.Duration
	halfWindow      time.Duration
	previousAt      time.Time
	previousCounter uint8
	sequential      int
	samples         int
	learned         bool
	missed          int
}

func (s *packetSchedule) observe(r reading) {
	// ATC can advertise the same sequence counter more than once. It is a
	// received packet, but it must not advance or reset cadence learning.
	if !s.previousAt.IsZero() && r.Counter == s.previousCounter {
		return
	}
	if !s.learned {
		if s.sequential == 0 || r.Counter != s.previousCounter+1 {
			s.anchor = r.Time
			s.sequential = 1
		} else {
			s.sequential++
			if s.sequential == 3 {
				s.interval = r.Time.Sub(s.anchor) / 2
				if s.interval <= 0 {
					s.sequential = 1
					s.anchor = r.Time
				} else {
					s.learned = true
					s.samples = 3
					s.halfWindow = 2 * time.Second
					s.missed = 0
				}
			}
		}
	} else if r.Counter != s.previousCounter+1 {
		s.learned = false
		s.sequential = 1
		s.samples = 1
		s.anchor = r.Time
		s.halfWindow = 2 * time.Second
		s.missed = 0
	} else {
		// Refine the period from all sequential packets seen since the original
		// anchor. This reduces timing noise without shifting the schedule phase.
		s.samples++
		s.interval = r.Time.Sub(s.anchor) / time.Duration(s.samples-1)
		s.missed = 0
		if s.halfWindow > 2*time.Second {
			s.halfWindow -= 500 * time.Millisecond
			if s.halfWindow < 2*time.Second {
				s.halfWindow = 2 * time.Second
			}
		}
	}
	s.previousCounter = r.Counter
	s.previousAt = r.Time
}

func (s *packetSchedule) nextWindow(now time.Time) (time.Time, time.Time) {
	if !s.learned {
		return now, time.Time{}
	}
	halfWindow := s.halfWindow
	if halfWindow <= 0 {
		halfWindow = 2 * time.Second
	}
	n := int64(now.Sub(s.anchor)/s.interval) + 1
	target := s.anchor.Add(time.Duration(n) * s.interval)
	return target.Add(-halfWindow), target.Add(halfWindow)
}

func collectLocal(ctx context.Context, interval time.Duration, save func(reading) error, events chan<- event, monitor bool) {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		events <- event{err: fmt.Errorf("Bluetooth: %w", err)}
		return
	}
	packets := make(chan reading, 64)
	schedule := packetSchedule{}
	deduper := packetCounterDeduper{}
	lastSaved := time.Time{}

	for {
		start, end := schedule.nextWindow(time.Now())
		if monitor {
			start, end = time.Now(), time.Time{}
		}
		if !end.IsZero() {
			resyncAt := schedule.previousAt.Add(3 * schedule.interval)
			if !time.Now().Before(resyncAt) {
				// We have gone three full expected intervals without a new
				// sequence number. Return to continuous scanning and relearn phase.
				schedule.learned = false
				schedule.sequential = 0
				start, end = time.Now(), time.Time{}
			} else if resyncAt.Before(end) {
				// Do not let the third missed window extend beyond the resync
				// threshold; switch to continuous scanning at that exact time.
				end = resyncAt
			}
		}
		if !end.IsZero() {
			if wait := time.Until(start); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}

		scanDone := make(chan error, 1)
		go func() {
			scanDone <- adapter.Scan(func(_ *bluetooth.Adapter, result bluetooth.ScanResult) {
				for _, data := range result.ServiceData() {
					if !strings.EqualFold(data.UUID.String(), "0000181a-0000-1000-8000-00805f9b34fb") {
						continue
					}
					r, ok := decodeATC(data.Data, int(result.RSSI))
					if !ok {
						continue
					}
					if !deduper.first(r) {
						continue
					}
					if monitor {
						log.Print(formatPacket(r))
						continue
					}
					select {
					case packets <- r:
					default:
					}
				}
			})
		}()

		windowReceived := false
		scanning := true
		for scanning {
			var stop <-chan time.Time
			var timer *time.Timer
			if !end.IsZero() {
				wait := time.Until(end)
				if wait <= 0 {
					_ = adapter.StopScan()
					<-scanDone
					break
				}
				timer = time.NewTimer(wait)
				stop = timer.C
			}
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				_ = adapter.StopScan()
				<-scanDone
				return
			case <-stop:
				_ = adapter.StopScan()
				<-scanDone
				scanning = false
			case r := <-packets:
				newCounter := schedule.previousAt.IsZero() || r.Counter != schedule.previousCounter
				wasLearned := schedule.learned
				schedule.observe(r)
				if newCounter {
					windowReceived = true
				}
				line := formatPacket(r)
				if !wasLearned && schedule.learned {
					line += fmt.Sprintf("  SYNC interval %s", schedule.interval)
				}
				log.Print(line)
				if lastSaved.IsZero() || r.Time.Sub(lastSaved) >= interval {
					if err := save(r); err != nil {
						events <- event{err: fmt.Errorf("saving reading: %w", err)}
						return
					}
					lastSaved = r.Time
					events <- event{reading: &r}
				}
				if !schedule.learned {
					end = time.Time{}
				} else if end.IsZero() {
					_ = adapter.StopScan()
					<-scanDone
					scanning = false
				}
			case err := <-scanDone:
				if err == nil {
					err = errors.New("scan stopped unexpectedly")
				}
				events <- event{err: fmt.Errorf("Bluetooth scan: %w", err)}
				return
			}
			if timer != nil && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		if schedule.learned && !windowReceived {
			schedule.missed++
			if schedule.halfWindow < 5*time.Second {
				schedule.halfWindow += time.Second
				if schedule.halfWindow > 5*time.Second {
					schedule.halfWindow = 5 * time.Second
				}
			}
			if schedule.missed >= 3 || time.Since(schedule.previousAt) >= 3*schedule.interval {
				schedule.learned = false
				schedule.sequential = 0
				schedule.samples = 0
				schedule.halfWindow = 2 * time.Second
			}
		}
	}
}

func proxyAddress(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, "6053")
}

func parseProxyHosts(configured, fallback string) ([]string, error) {
	if strings.TrimSpace(configured) == "" {
		return []string{fallback}, nil
	}
	var hosts []string
	seen := make(map[string]struct{})
	for _, host := range strings.Split(configured, ",") {
		host = strings.TrimSpace(host)
		if host == "" {
			return nil, errors.New("--proxies must contain comma-separated, non-empty host names")
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

func subscribeProxyAdvertisements(client *esphome.Client, handler func(proto.Message)) (func(), error) {
	removeRaw := client.On(93, handler)
	removeLegacy := client.On(67, handler)
	if err := client.SendMessage(&pb.SubscribeBluetoothLEAdvertisementsRequest{Flags: 1}, 66); err != nil {
		removeRaw()
		removeLegacy()
		return nil, fmt.Errorf("requesting raw BLE advertisements: %w", err)
	}
	return func() {
		removeRaw()
		removeLegacy()
		_ = client.SendMessage(&pb.UnsubscribeBluetoothLEAdvertisementsRequest{}, 87)
	}, nil
}

func requestPassiveProxyScan(client *esphome.Client) {
	info, err := client.DeviceInfo()
	if err != nil {
		log.Printf("Could not read proxy capabilities; using its configured scan mode: %v", err)
		return
	}
	const scannerModeFeature = uint32(1 << 6)
	if info.GetBluetoothProxyFeatureFlags()&scannerModeFeature == 0 {
		log.Print("Proxy does not advertise remote scan-mode control; set its ESPHome BLE scan parameters to passive mode")
		return
	}
	if err := client.BluetoothScannerSetMode(pb.BluetoothScannerMode_BLUETOOTH_SCANNER_MODE_PASSIVE); err != nil {
		log.Printf("Could not request passive scanning; using proxy's configured mode: %v", err)
		return
	}
	log.Print("Requested passive BLE scanning on the proxy")
}

func decodeLegacyProxyAdvertisement(adv *pb.BluetoothLEAdvertisementResponse) (reading, bool) {
	for _, service := range adv.ServiceData {
		if strings.EqualFold(service.Uuid, "0000181a-0000-1000-8000-00805f9b34fb") || strings.EqualFold(service.Uuid, "181a") {
			return decodeATC(service.Data, int(adv.Rssi))
		}
	}
	return reading{}, false
}

type scanServiceData struct {
	UUID string
	Data []byte
}

type scanManufacturerData struct {
	ID   string
	Data []byte
}

type scanAdvertisement struct {
	Address          string
	RSSI             int
	Raw              []byte
	ServiceUUIDs     []string
	ServiceData      []scanServiceData
	ManufacturerData []scanManufacturerData
}

func canonicalScanUUID(uuid string) string {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	if len(uuid) == 36 && strings.HasPrefix(uuid, "0000") && strings.HasSuffix(uuid, "-0000-1000-8000-00805f9b34fb") {
		return uuid[4:8]
	}
	if len(uuid) == 8 && strings.HasPrefix(uuid, "0000") {
		return uuid[4:]
	}
	return uuid
}

func littleEndianUUID128(data []byte) string {
	if len(data) != 16 {
		return ""
	}
	value := append([]byte(nil), data...)
	for left, right := 0, len(value)-1; left < right; left, right = left+1, right-1 {
		value[left], value[right] = value[right], value[left]
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func formatScanAdvertisement(adv scanAdvertisement) (string, string) {
	address := adv.Address
	var serviceUUIDs, serviceData, manufacturerData, decodedReadings []string
	key := address
	if len(adv.ServiceUUIDs) > 0 {
		for _, uuid := range adv.ServiceUUIDs {
			uuid = canonicalScanUUID(uuid)
			serviceUUIDs = append(serviceUUIDs, uuid)
			key += "|u:" + uuid
		}
	}
	for _, service := range adv.ServiceData {
		uuid := canonicalScanUUID(service.UUID)
		data := append([]byte(nil), service.Data...)
		key += fmt.Sprintf("|s:%s:%x", uuid, data)
		serviceData = append(serviceData, fmt.Sprintf("%s=%x", uuid, data))
		if advertised := advertisedScanMAC(uuid, data); advertised != "" {
			address = advertised
		}
		if summary := decodeBroadcastTemperature(uuid, data); summary != "" {
			decodedReadings = append(decodedReadings, summary)
		}
	}
	for _, manufacturer := range adv.ManufacturerData {
		data := append([]byte(nil), manufacturer.Data...)
		key += fmt.Sprintf("|m:%s:%x", manufacturer.ID, data)
		manufacturerData = append(manufacturerData, fmt.Sprintf("%s=%x", manufacturer.ID, data))
	}
	if len(adv.ServiceUUIDs)+len(adv.ServiceData)+len(adv.ManufacturerData) == 0 {
		key += fmt.Sprintf("|raw:%x", adv.Raw)
	}
	if len(decodedReadings) == 0 {
		decodedReadings = []string{"none detected"}
	}
	raw := "unavailable"
	if len(adv.Raw) > 0 {
		raw = fmt.Sprintf("%x", adv.Raw)
	}
	line := fmt.Sprintf("Address: %s | Scanner ID: %s | RSSI: %d dBm | Raw AD: %s | Service UUIDs: %s | Service data: %s | Manufacturer data: %s | Decoded: %s",
		address, adv.Address, adv.RSSI, raw, scanFieldValue(serviceUUIDs), scanFieldValue(serviceData), scanFieldValue(manufacturerData), strings.Join(decodedReadings, "; "))
	return key, line
}

func scanFieldValue(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func advertisedScanMAC(uuid string, data []byte) string {
	uuid = canonicalScanUUID(uuid)
	switch uuid {
	case "181a":
		if len(data) >= 6 {
			return fmt.Sprintf("%02X%02X%02X%02X%02X%02X", data[0], data[1], data[2], data[3], data[4], data[5])
		}
	case "fe95":
		if len(data) < 11 || binary.LittleEndian.Uint16(data[:2])&0x10 == 0 {
			return ""
		}
		return fmt.Sprintf("%02X%02X%02X%02X%02X%02X", data[10], data[9], data[8], data[7], data[6], data[5])
	}
	return ""
}

func decodeBroadcastTemperature(uuid string, data []byte) string {
	uuid = canonicalScanUUID(uuid)
	switch uuid {
	case "181a":
		switch len(data) {
		case 13: // ATC service data
			temp := float64(int16(binary.BigEndian.Uint16(data[6:8]))) / 10
			return fmt.Sprintf("ATC temperature %.1f°C, humidity %d%%, battery %d%% (%d mV)", temp, data[8], data[9], binary.BigEndian.Uint16(data[10:12]))
		case 15: // PVVX custom service data
			temp := float64(int16(binary.LittleEndian.Uint16(data[6:8]))) / 100
			humidity := float64(binary.LittleEndian.Uint16(data[8:10])) / 100
			return fmt.Sprintf("PVVX temperature %.2f°C, humidity %.2f%%, battery %d%% (%d mV)", temp, humidity, data[12], binary.LittleEndian.Uint16(data[10:12]))
		}
	case "fcd2":
		return decodeBTHomeTemperature(data)
	case "fe95":
		decoded, _ := decodeXiaomiMiBeacon(data)
		return decoded
	}
	return ""
}

func decodeXiaomiMiBeacon(data []byte) (decoded, advertisedMAC string) {
	if len(data) < 5 {
		return "", ""
	}
	control := binary.LittleEndian.Uint16(data[:2])
	productID := binary.LittleEndian.Uint16(data[2:4])
	version := (control >> 12) & 0x0f
	model := map[uint16]string{0x01aa: "LYWSDCGQ", 0x045b: "LYWSD02", 0x055b: "LYWSD03MMC", 0x066f: "CGDK2", 0x06d3: "MHO-C303"}[productID]
	label := fmt.Sprintf("Xiaomi MiBeacon v%d", version)
	if model != "" {
		label += " " + model
	}
	if control&0x08 != 0 {
		return label + " (encrypted; bind key required to decode temperature)", ""
	}
	offset := 5
	if control&0x10 != 0 {
		if len(data) < offset+6 {
			return "", ""
		}
		advertisedMAC = fmt.Sprintf("%02X%02X%02X%02X%02X%02X", data[offset+5], data[offset+4], data[offset+3], data[offset+2], data[offset+1], data[offset])
		offset += 6
	}
	if control&0x20 != 0 {
		if len(data) <= offset {
			return "", advertisedMAC
		}
		offset++ // capability byte
	}
	if control&0x40 == 0 {
		return label + " (no temperature object in this advertisement)", advertisedMAC
	}
	var values []string
	for offset+3 <= len(data) {
		objectID := binary.LittleEndian.Uint16(data[offset : offset+2])
		length := int(data[offset+2])
		offset += 3
		if length == 0 || offset+length > len(data) {
			break
		}
		payload := data[offset : offset+length]
		offset += length
		switch objectID {
		case 0x1004:
			if length == 2 {
				values = append(values, fmt.Sprintf("temperature %.1f°C", float64(int16(binary.LittleEndian.Uint16(payload)))/10))
			}
		case 0x1006:
			if length == 2 {
				values = append(values, fmt.Sprintf("humidity %.1f%%", float64(binary.LittleEndian.Uint16(payload))/10))
			}
		case 0x100a:
			if length == 1 {
				values = append(values, fmt.Sprintf("battery %d%%", payload[0]))
			}
		case 0x100d:
			if length == 4 {
				values = append(values, fmt.Sprintf("temperature %.1f°C, humidity %.1f%%",
					float64(int16(binary.LittleEndian.Uint16(payload[:2])))/10,
					float64(binary.LittleEndian.Uint16(payload[2:]))/10))
			}
		}
	}
	if len(values) == 0 {
		return label + " (no supported temperature object)", advertisedMAC
	}
	return label + " " + strings.Join(values, ", "), advertisedMAC
}

func decodeSensorObservations(adv scanAdvertisement) []sensorObservation {
	observedAt := time.Now().UTC()
	observations := make([]sensorObservation, 0, len(adv.ServiceData))
	for _, service := range adv.ServiceData {
		uuid := canonicalScanUUID(service.UUID)
		data := service.Data
		deviceID := adv.Address
		if mac := advertisedScanMAC(uuid, data); mac != "" {
			deviceID = mac
		}
		observation := sensorObservation{Time: observedAt, DeviceID: deviceID, RSSI: adv.RSSI}
		switch {
		case uuid == "181a" && len(data) == 13:
			observation.Format = "ATC"
			observation.Temperature = floatPtr(float64(int16(binary.BigEndian.Uint16(data[6:8]))) / 10)
			observation.Humidity = floatPtr(float64(data[8]))
			observation.Battery = floatPtr(float64(data[9]))
			observation.Millivolts = floatPtr(float64(binary.BigEndian.Uint16(data[10:12])))
			counter := data[12]
			observation.Counter = &counter
		case uuid == "181a" && len(data) == 15:
			observation.Format = "PVVX"
			observation.Temperature = floatPtr(float64(int16(binary.LittleEndian.Uint16(data[6:8]))) / 100)
			observation.Humidity = floatPtr(float64(binary.LittleEndian.Uint16(data[8:10])) / 100)
			observation.Battery = floatPtr(float64(data[12]))
			observation.Millivolts = floatPtr(float64(binary.LittleEndian.Uint16(data[10:12])))
		case uuid == "fcd2":
			observation = decodeBTHomeObservation(data, observation)
		case uuid == "fe95":
			observation = decodeXiaomiObservation(data, observation)
		default:
			continue
		}
		if observation.Format != "" && (observation.Temperature != nil || observation.Humidity != nil || observation.Battery != nil || observation.Millivolts != nil) {
			observation.DeviceID = normalizeSensorDeviceID(observation.DeviceID)
			observations = append(observations, observation)
		}
	}
	return observations
}

func decodeBTHomeObservation(data []byte, observation sensorObservation) sensorObservation {
	if len(data) < 2 || data[0]>>5 != 2 || data[0]&1 != 0 {
		return sensorObservation{}
	}
	observation.Format = "BTHome v2"
	for i := 1; i < len(data); {
		objectID := data[i]
		i++
		length, signed := 0, false
		switch objectID {
		case 0x00, 0x01, 0x2e:
			length = 1
		case 0x02, 0x03, 0x0c:
			length = 2
			signed = objectID == 0x02
		default:
			return observation
		}
		if i+length > len(data) {
			return sensorObservation{}
		}
		var value int64
		if length == 1 {
			value = int64(data[i])
		} else if signed {
			value = int64(int16(binary.LittleEndian.Uint16(data[i : i+length])))
		} else {
			value = int64(binary.LittleEndian.Uint16(data[i : i+length]))
		}
		i += length
		switch objectID {
		case 0x00:
			counter := uint8(value)
			observation.Counter = &counter
		case 0x01:
			observation.Battery = floatPtr(float64(value))
		case 0x02:
			observation.Temperature = floatPtr(float64(value) / 100)
		case 0x03:
			observation.Humidity = floatPtr(float64(value) / 100)
		case 0x0c:
			observation.Millivolts = floatPtr(float64(value))
		case 0x2e:
			observation.Humidity = floatPtr(float64(value))
		}
	}
	return observation
}

func decodeXiaomiObservation(data []byte, observation sensorObservation) sensorObservation {
	if len(data) < 5 {
		return sensorObservation{}
	}
	control := binary.LittleEndian.Uint16(data[:2])
	if control&0x08 != 0 {
		return sensorObservation{}
	}
	observation.Format = "Xiaomi MiBeacon"
	offset := 5
	if control&0x10 != 0 {
		if len(data) < offset+6 {
			return sensorObservation{}
		}
		observation.DeviceID = fmt.Sprintf("%02X%02X%02X%02X%02X%02X", data[offset+5], data[offset+4], data[offset+3], data[offset+2], data[offset+1], data[offset])
		offset += 6
	}
	if control&0x20 != 0 {
		if len(data) <= offset {
			return sensorObservation{}
		}
		offset++
	}
	if control&0x40 == 0 {
		return sensorObservation{}
	}
	for offset+3 <= len(data) {
		objectID := binary.LittleEndian.Uint16(data[offset : offset+2])
		length := int(data[offset+2])
		offset += 3
		if length == 0 || offset+length > len(data) {
			return sensorObservation{}
		}
		payload := data[offset : offset+length]
		offset += length
		switch objectID {
		case 0x1004:
			if length == 2 {
				observation.Temperature = floatPtr(float64(int16(binary.LittleEndian.Uint16(payload))) / 10)
			}
		case 0x1006:
			if length == 2 {
				observation.Humidity = floatPtr(float64(binary.LittleEndian.Uint16(payload)) / 10)
			}
		case 0x100a:
			if length == 1 {
				observation.Battery = floatPtr(float64(payload[0]))
			}
		case 0x100d:
			if length == 4 {
				observation.Temperature = floatPtr(float64(int16(binary.LittleEndian.Uint16(payload[:2]))) / 10)
				observation.Humidity = floatPtr(float64(binary.LittleEndian.Uint16(payload[2:])) / 10)
			}
		}
	}
	counter := data[4]
	observation.Counter = &counter
	return observation
}

func decodeBTHomeTemperature(data []byte) string {
	if len(data) < 2 || data[0]>>5 != 2 || data[0]&1 != 0 {
		return ""
	}
	var fields []string
	hasTemperature := false
	for i := 1; i < len(data); {
		objectID := data[i]
		i++
		length, signed, label, scale := 0, false, "", 1.0
		switch objectID {
		case 0x00:
			length, label = 1, "packet"
		case 0x01:
			length, label = 1, "battery"
		case 0x02:
			length, signed, label, scale = 2, true, "temperature", 100
		case 0x03:
			length, label, scale = 2, "humidity", 100
		case 0x0c:
			length, label = 2, "voltage"
		case 0x2e:
			length, label = 1, "humidity"
		default:
			i = len(data)
			continue
		}
		if i+length > len(data) {
			return ""
		}
		var value int64
		if length == 1 {
			value = int64(data[i])
		} else if signed {
			value = int64(int16(binary.LittleEndian.Uint16(data[i : i+length])))
		} else {
			value = int64(binary.LittleEndian.Uint16(data[i : i+length]))
		}
		i += length
		if label == "temperature" {
			hasTemperature = true
		}
		if scale == 1 {
			fields = append(fields, fmt.Sprintf("%s %d", label, value))
		} else {
			fields = append(fields, fmt.Sprintf("%s %.2f", label, float64(value)/scale))
		}
	}
	if !hasTemperature {
		return ""
	}
	return "BTHome v2 " + strings.Join(fields, ", ")
}

func parseRawScanAdvertisement(data []byte) (serviceUUIDs []string, services []scanServiceData, manufacturers []scanManufacturerData) {
	for len(data) > 0 {
		fieldLen := int(data[0])
		if fieldLen == 0 || fieldLen+1 > len(data) {
			break
		}
		fieldType := data[1]
		field := data[2 : fieldLen+1]
		switch fieldType {
		case 0x02, 0x03:
			for len(field) >= 2 {
				uuid := binary.LittleEndian.Uint16(field[:2])
				serviceUUIDs = append(serviceUUIDs, fmt.Sprintf("%04x", uuid))
				field = field[2:]
			}
		case 0x04, 0x05:
			for len(field) >= 4 {
				serviceUUIDs = append(serviceUUIDs, fmt.Sprintf("%08x", binary.LittleEndian.Uint32(field[:4])))
				field = field[4:]
			}
		case 0x06, 0x07:
			for len(field) >= 16 {
				serviceUUIDs = append(serviceUUIDs, littleEndianUUID128(field[:16]))
				field = field[16:]
			}
		case 0x16:
			if len(field) >= 2 {
				services = append(services, scanServiceData{UUID: fmt.Sprintf("%04x", binary.LittleEndian.Uint16(field[:2])), Data: append([]byte(nil), field[2:]...)})
			}
		case 0x20:
			if len(field) >= 4 {
				services = append(services, scanServiceData{UUID: fmt.Sprintf("%08x", binary.LittleEndian.Uint32(field[:4])), Data: append([]byte(nil), field[4:]...)})
			}
		case 0x21:
			if len(field) >= 16 {
				services = append(services, scanServiceData{UUID: littleEndianUUID128(field[:16]), Data: append([]byte(nil), field[16:]...)})
			}
		case 0xff:
			if len(field) >= 2 {
				manufacturers = append(manufacturers, scanManufacturerData{ID: fmt.Sprintf("%04x", binary.LittleEndian.Uint16(field[:2])), Data: append([]byte(nil), field[2:]...)})
			}
		}
		data = data[fieldLen+1:]
	}
	return
}

type scanOutput struct {
	mu       sync.Mutex
	indexes  map[string]int
	readings []scanAdvertisement
}

func newScanOutput() *scanOutput {
	return &scanOutput{indexes: make(map[string]int)}
}

func (out *scanOutput) add(adv scanAdvertisement) {
	key, line := formatScanAdvertisement(adv)
	out.mu.Lock()
	defer out.mu.Unlock()
	if index, exists := out.indexes[key]; exists {
		previous := &out.readings[index]
		enriched := false
		if len(previous.Raw) == 0 && len(adv.Raw) > 0 {
			previous.Raw = append([]byte(nil), adv.Raw...)
			enriched = true
		}
		if adv.RSSI > previous.RSSI {
			previous.RSSI = adv.RSSI
		}
		if enriched && index < 150 {
			_, line = formatScanAdvertisement(*previous)
			fmt.Println(line)
		}
		return
	}
	index := len(out.readings)
	out.indexes[key] = index
	out.readings = append(out.readings, adv)
	if index < 150 {
		fmt.Println(line)
	}
}

func (out *scanOutput) printResults() {
	out.mu.Lock()
	defer out.mu.Unlock()
	limit := min(len(out.readings), 150)
	if len(out.readings) > limit {
		fmt.Printf("... (%d additional distinct advertisements omitted)\n", len(out.readings)-limit)
	}
	fmt.Printf("Distinct advertisements: %d\n", len(out.readings))
	if len(out.readings) == 0 {
		fmt.Println("No BLE advertisements received; check adapter permissions, scan status, and sensor range.")
	}
}

func scanResultError(err error) error {
	if errors.Is(err, context.Canceled) {
		fmt.Println("Scan interrupted.")
		return nil
	}
	return err
}

func scanProxy(ctx context.Context, host, encryptionKey string, duration time.Duration) error {
	address := proxyAddress(host)
	opts := []esphome.Option{esphome.WithClientInfo("TempLogger BLE scanner")}
	if encryptionKey != "" {
		opts = append(opts, esphome.WithEncryptionKey(encryptionKey))
	}
	client, err := esphome.DialWithContext(ctx, address, 5*time.Second, opts...)
	if err != nil {
		return fmt.Errorf("connecting to ESPHome proxy %s: %w", address, err)
	}
	defer client.Close()
	requestPassiveProxyScan(client)

	output := newScanOutput()
	fmt.Printf("Scanning BLE advertisements via ESPHome proxy %s for %s...\n", address, duration)
	unsubscribe, err := subscribeProxyAdvertisements(client, func(msg proto.Message) {
		switch batch := msg.(type) {
		case *pb.BluetoothLERawAdvertisementsResponse:
			for _, adv := range batch.Advertisements {
				uuids, services, manufacturers := parseRawScanAdvertisement(adv.Data)
				output.add(scanAdvertisement{
					Address: fmt.Sprintf("%012X", adv.Address), RSSI: int(adv.Rssi),
					Raw: append([]byte(nil), adv.Data...), ServiceUUIDs: uuids,
					ServiceData: services, ManufacturerData: manufacturers,
				})
			}
		case *pb.BluetoothLEAdvertisementResponse:
			adv := scanAdvertisement{Address: fmt.Sprintf("%012X", batch.Address), RSSI: int(batch.Rssi)}
			adv.ServiceUUIDs = append(adv.ServiceUUIDs, batch.ServiceUuids...)
			for _, service := range batch.ServiceData {
				adv.ServiceData = append(adv.ServiceData, scanServiceData{UUID: service.Uuid, Data: append([]byte(nil), service.Data...)})
			}
			for _, manufacturer := range batch.ManufacturerData {
				adv.ManufacturerData = append(adv.ManufacturerData, scanManufacturerData{ID: manufacturer.Uuid, Data: append([]byte(nil), manufacturer.Data...)})
			}
			output.add(adv)
		}
	})
	if err != nil {
		return err
	}
	active := true
	defer func() {
		if active {
			unsubscribe()
		}
	}()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	var scanErr error
	select {
	case <-ctx.Done():
		scanErr = ctx.Err()
	case <-client.Done():
		scanErr = errors.New("proxy disconnected during scan")
	case <-timer.C:
	}
	unsubscribe()
	active = false
	output.printResults()
	return scanResultError(scanErr)
}

func scanLocalBLE(ctx context.Context, duration time.Duration) error {
	adapter := bluetooth.DefaultAdapter
	if err := adapter.Enable(); err != nil {
		return fmt.Errorf("enabling local Bluetooth adapter: %w", err)
	}

	output := newScanOutput()
	fmt.Printf("Scanning BLE advertisements via local Bluetooth adapter for %s...\n", duration)
	scanDone := make(chan error, 1)
	go func() {
		scanDone <- adapter.Scan(func(_ *bluetooth.Adapter, result bluetooth.ScanResult) {
			adv := scanAdvertisement{
				Address: result.Address.String(), RSSI: int(result.RSSI),
				Raw: append([]byte(nil), result.Bytes()...),
			}
			for _, uuid := range result.ServiceUUIDs() {
				adv.ServiceUUIDs = append(adv.ServiceUUIDs, uuid.String())
			}
			for _, service := range result.ServiceData() {
				adv.ServiceData = append(adv.ServiceData, scanServiceData{UUID: service.UUID.String(), Data: append([]byte(nil), service.Data...)})
			}
			for _, manufacturer := range result.ManufacturerData() {
				adv.ManufacturerData = append(adv.ManufacturerData, scanManufacturerData{ID: fmt.Sprintf("%04x", manufacturer.CompanyID), Data: append([]byte(nil), manufacturer.Data...)})
			}
			output.add(adv)
		})
	}()

	timer := time.NewTimer(duration)
	defer timer.Stop()
	var scanErr error
	select {
	case <-ctx.Done():
		_ = adapter.StopScan()
		<-scanDone
		scanErr = ctx.Err()
	case err := <-scanDone:
		if err == nil {
			scanErr = errors.New("local Bluetooth scan stopped unexpectedly")
		} else {
			scanErr = fmt.Errorf("local Bluetooth scan: %w", err)
		}
	case <-timer.C:
		_ = adapter.StopScan()
		if err := <-scanDone; err != nil {
			scanErr = fmt.Errorf("stopping local Bluetooth scan: %w", err)
		}
	}
	output.printResults()
	return scanResultError(scanErr)
}

// readSensorConfig connects briefly over the ESPHome proxy and asks the ATC
// firmware for its current (read-only) configuration block.
func readSensorConfig(ctx context.Context, host, encryptionKey string, duration time.Duration, resetATC1441 bool) error {
	address := proxyAddress(host)
	opts := []esphome.Option{esphome.WithClientInfo("TempLogger sensor config reader")}
	if encryptionKey != "" {
		opts = append(opts, esphome.WithEncryptionKey(encryptionKey))
	}
	client, err := esphome.DialWithContext(ctx, address, 5*time.Second, opts...)
	if err != nil {
		return fmt.Errorf("connecting to ESPHome proxy %s: %w", address, err)
	}
	defer client.Close()
	requestPassiveProxyScan(client)
	type bleDevice struct {
		address     uint64
		addressType uint32
	}
	device := make(chan bleDevice, 1)
	unsubscribe, err := subscribeProxyAdvertisements(client, func(msg proto.Message) {
		batch, ok := msg.(*pb.BluetoothLERawAdvertisementsResponse)
		if !ok {
			return
		}
		for _, adv := range batch.Advertisements {
			if _, matches := decodeProxyAdvertisement(adv.Data, int(adv.Rssi)); matches {
				select {
				case device <- bleDevice{address: adv.Address, addressType: adv.AddressType}:
				default:
				}
				return
			}
		}
	})
	if err != nil {
		return err
	}
	defer unsubscribe()
	fmt.Printf("Waiting up to %s for ATC_EA80E1 via %s...\n", duration, address)
	var sensor bleDevice
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-client.Done():
		return errors.New("proxy disconnected while locating sensor")
	case <-timer.C:
		return errors.New("sensor advertisement not seen; cannot connect to read configuration")
	case sensor = <-device:
	}
	deviceAddress := sensor.address
	fmt.Printf("Sensor found at BLE address %012X (address type %d); connecting briefly to read settings...\n", deviceAddress, sensor.addressType)
	if err := connectProxyDevice(client, deviceAddress, sensor.addressType); err != nil {
		return fmt.Errorf("connecting to sensor: %w; if the sensor has a button configured for Connect, short-press it and rerun --sensor-config within 80 seconds", err)
	}
	defer func() { _ = client.BluetoothDisconnect(deviceAddress) }()
	services, err := client.BluetoothGATTGetServices(deviceAddress)
	if err != nil {
		return fmt.Errorf("discovering sensor GATT services: %w", err)
	}
	var configCharacteristic *pb.BluetoothGATTCharacteristic
	var discovered []string
	for _, service := range services {
		discovered = append(discovered, fmt.Sprintf("service short=%#x uuid=%#v", service.GetShortUuid(), service.GetUuid()))
		for _, characteristic := range service.GetCharacteristics() {
			discovered = append(discovered, fmt.Sprintf("  characteristic short=%#x uuid=%#v handle=%d", characteristic.GetShortUuid(), characteristic.GetUuid(), characteristic.GetHandle()))
			if gattUUID16Matches(characteristic.GetShortUuid(), characteristic.GetUuid(), 0x1F1F) {
				configCharacteristic = characteristic
				break
			}
		}
	}
	if configCharacteristic == nil {
		return fmt.Errorf("ATC settings characteristic 0x1F1F was not found; discovered GATT UUIDs:\n%s", strings.Join(discovered, "\n"))
	}
	configHandle := configCharacteristic.GetHandle()
	writeWithResponse := configCharacteristic.GetProperties()&0x08 != 0 || configCharacteristic.GetProperties()&0x04 == 0
	fmt.Printf("Settings characteristic handle 0x%X, properties 0x%X; write with response=%t\n", configHandle, configCharacteristic.GetProperties(), writeWithResponse)
	if resetATC1441 {
		if err := client.BluetoothGATTWrite(deviceAddress, configHandle, []byte{0xDE}, writeWithResponse); err != nil {
			return fmt.Errorf("sending ATC1441 restore-defaults command DE: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
		if err := client.BluetoothGATTWrite(deviceAddress, configHandle, []byte{0xDF}, writeWithResponse); err != nil {
			return fmt.Errorf("saving ATC1441 default settings with DF: %w", err)
		}
		fmt.Println("Sent ATC1441 DE (restore settings to defaults), then DF (save settings to flash). The firmware does not acknowledge these one-way commands.")
		return nil
	}
	config := make(chan []byte, 1)
	stopNotify, err := client.BluetoothGATTNotify(deviceAddress, configHandle, true, func(data []byte) {
		log.Printf("Sensor GATT notification on handle 0x%X: %X", configHandle, data)
		if len(data) > 0 && data[0] == 0x55 {
			select {
			case config <- append([]byte(nil), data...):
			default:
			}
		}
	})
	if err != nil {
		return fmt.Errorf("subscribing to sensor config notifications: %w", err)
	}
	defer func() {
		if stopNotify != nil {
			stopNotify()
		}
	}()
	if err := client.BluetoothGATTWrite(deviceAddress, configHandle, []byte{0x55}, writeWithResponse); err != nil {
		return fmt.Errorf("requesting current sensor config: %w", err)
	}
	var data []byte
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-client.Done():
		return errors.New("proxy disconnected while reading sensor config")
	case <-time.After(10 * time.Second):
		return errors.New("timed out waiting for sensor config response")
	case data = <-config:
	}
	if len(data) < 10 {
		return fmt.Errorf("sensor returned short config response (%d bytes): %x", len(data), data)
	}
	fw := data[1]
	adv := time.Duration(data[6]) * 625 * time.Millisecond / 10 // 62.5 ms units
	measure := adv * time.Duration(data[7])
	fmt.Printf("Firmware: %d.%d (raw 0x%02X)\n", fw>>4, fw&0x0f, fw)
	fmt.Printf("Broadcast interval: %s (raw %d × 62.5 ms)\n", adv, data[6])
	fmt.Printf("Temperature measurement interval: %s (%d × broadcast interval)\n", measure, data[7])
	fmt.Printf("Flags: 0x%02X / 0x%02X; low-power measurement: %t; transmit-during-measurement: %t\n", data[2], data[3], data[2]&0x80 != 0, data[2]&0x40 != 0)
	fmt.Printf("RF TX power setting: %d; connect latency setting: %d\n", data[8], data[9])
	if len(data) >= 13 {
		fmt.Printf("Averaging-to-flash interval: %d × measurement interval\n", data[12])
	}
	fmt.Printf("Raw config packet: %X\n", data)
	return nil
}

func gattUUID16Matches(shortUUID uint32, uuid []uint64, wanted uint16) bool {
	if shortUUID != 0 {
		return shortUUID == uint32(wanted)
	}
	// Older ESPHome API versions expand 16-bit UUIDs into the Bluetooth base
	// UUID and encode its two halves into these uint64 fields.
	return len(uuid) >= 2 && uint32(uuid[0]>>32) == uint32(wanted) && uint32(uuid[0]) == 0x00001000 && uuid[1] == 0x800000805F9B34FB
}

// connectProxyDevice sends the address type observed in the proxy's raw BLE
// advertisement. ESPHome requires this for connection requests to address
// types that cannot be inferred from the six-byte address alone.
func connectProxyDevice(client *esphome.Client, address uint64, addressType uint32) error {
	response := make(chan *pb.BluetoothDeviceConnectionResponse, 1)
	remove := client.On(69, func(msg proto.Message) {
		resp, ok := msg.(*pb.BluetoothDeviceConnectionResponse)
		if !ok || resp.GetAddress() != address {
			return
		}
		select {
		case response <- resp:
		default:
		}
	})
	defer remove()
	req := &pb.BluetoothDeviceRequest{
		Address:        address,
		RequestType:    pb.BluetoothDeviceRequestType_BLUETOOTH_DEVICE_REQUEST_TYPE_CONNECT_V3_WITHOUT_CACHE,
		HasAddressType: true,
		AddressType:    addressType,
	}
	if err := client.SendMessage(req, 68); err != nil {
		return fmt.Errorf("sending BLE connection request: %w", err)
	}
	select {
	case resp := <-response:
		if !resp.GetConnected() {
			return fmt.Errorf("proxy rejected connection (error code %d)", resp.GetError())
		}
		return nil
	case <-time.After(30 * time.Second):
		return errors.New("timed out waiting for BLE connection response")
	case <-client.Done():
		return errors.New("proxy disconnected while connecting")
	}
}

func waitProxyReconnect(ctx context.Context, address string) bool {
	log.Printf("Proxy connection lost; reconnecting to %s in 5 seconds", address)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
	}
	log.Printf("Attempting ESPHome proxy reconnect to %s", address)
	return true
}

func streamProxyLogs(ctx context.Context, host, encryptionKey string) error {
	address := proxyAddress(host)
	fmt.Printf("Streaming ESPHome logs from %s; press Ctrl-C to stop.\n", address)
	for ctx.Err() == nil {
		opts := []esphome.Option{esphome.WithClientInfo("TempLogger proxy log viewer")}
		if encryptionKey != "" {
			opts = append(opts, esphome.WithEncryptionKey(encryptionKey))
		}
		client, err := esphome.DialWithContext(ctx, address, 5*time.Second, opts...)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("ESPHome Bluetooth proxy %s: failed to connect: %v (check that TCP 6053 is reachable and the API encryption key is correct)", address, err)
			if !waitProxyReconnect(ctx, address) {
				break
			}
			continue
		}
		fmt.Printf("Connected to ESPHome proxy %s; subscribing to verbose device logs.\n", address)
		unsubscribe, err := client.SubscribeLogs(pb.LogLevel_LOG_LEVEL_VERBOSE, func(msg *pb.SubscribeLogsResponse) {
			fmt.Printf("[%s] %s", msg.Level, string(msg.Message))
			if len(msg.Message) == 0 || msg.Message[len(msg.Message)-1] != '\n' {
				fmt.Println()
			}
		})
		if err != nil {
			log.Printf("Subscribing to proxy logs failed: %v", err)
			_ = client.Close()
			if !waitProxyReconnect(ctx, address) {
				break
			}
			continue
		}
		select {
		case <-ctx.Done():
			unsubscribe()
			_ = client.Close()
			return nil
		case <-client.Done():
			unsubscribe()
			_ = client.Close()
			log.Printf("ESPHome proxy log connection closed")
		}
		if !waitProxyReconnect(ctx, address) {
			break
		}
	}
	return nil
}

func collectProxy(ctx context.Context, host, encryptionKey string, interval time.Duration, save func(reading) error, events chan<- event, monitor, logPackets bool) {
	address := proxyAddress(host)
	packets := make(chan reading, 128)
	lastSaved := time.Time{}
	deduper := packetCounterDeduper{}
	log.Printf("Connecting to ESPHome Bluetooth proxy at %s", address)

	for {
		if ctx.Err() != nil {
			return
		}
		opts := []esphome.Option{esphome.WithClientInfo("TempLogger Bluetooth collector")}
		if encryptionKey != "" {
			opts = append(opts, esphome.WithEncryptionKey(encryptionKey))
		}
		client, err := esphome.DialWithContext(ctx, address, 5*time.Second, opts...)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("ESPHome Bluetooth proxy %s: failed to connect: %v (check that TCP 6053 is reachable and the API encryption key is correct)", address, err)
			if !waitProxyReconnect(ctx, address) {
				return
			}
			continue
		}

		log.Printf("Connected to ESPHome Bluetooth proxy %s", address)
		connectedAt := time.Now()
		var lastAdvertisement atomic.Int64
		lastAdvertisement.Store(connectedAt.UnixNano())
		requestPassiveProxyScan(client)
		handler := func(msg proto.Message) {
			handle := func(r reading, ok bool) {
				if !ok {
					return
				}
				lastAdvertisement.Store(r.Time.UnixNano())
				if !deduper.first(r) {
					return
				}
				if monitor || logPackets {
					log.Printf("ATC_EA80E1 #%d  %.1f°C  %d%% RH  battery %d%% (%.3f V)  RSSI %d dBm",
						r.Counter, r.Temp, r.Humidity, r.Battery, float64(r.Millivolts)/1000, r.RSSI)
				}
				if monitor {
					return
				}
				select {
				case packets <- r:
				default:
				}
			}
			switch batch := msg.(type) {
			case *pb.BluetoothLERawAdvertisementsResponse:
				for _, adv := range batch.Advertisements {
					handle(decodeProxyAdvertisement(adv.Data, int(adv.Rssi)))
				}
			case *pb.BluetoothLEAdvertisementResponse:
				handle(decodeLegacyProxyAdvertisement(batch))
			}
		}
		unsubscribe, err := subscribeProxyAdvertisements(client, handler)
		if err != nil {
			if ctx.Err() != nil {
				_ = client.Close()
				return
			}
			log.Printf("Subscribing to BLE advertisements via %s failed: %v", address, err)
			_ = client.Close()
			if !waitProxyReconnect(ctx, address) {
				return
			}
			continue
		}
		log.Printf("Subscribed to BLE advertisements via %s; waiting for ATC_EA80E1 packets", address)

		connected := true
		idleTicker := time.NewTicker(30 * time.Second)
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
				last := time.Unix(0, lastAdvertisement.Load())
				idle := time.Since(last).Round(time.Second)
				if idle >= 30*time.Second {
					log.Printf("No valid ATC_EA80E1 packet received for %s; refreshing BLE subscription", idle)
					unsubscribe()
					// Another BLE client may have replaced this subscription while the
					// API connection stayed open. Re-register the handlers and send a
					// fresh subscribe request without dropping the proxy connection.
					lastAdvertisement.Store(time.Now().UnixNano())
					unsubscribe, err = subscribeProxyAdvertisements(client, handler)
					if err != nil {
						log.Printf("Refreshing BLE advertisement subscription via %s failed: %v", address, err)
						connected = false
					}
				}
			case r := <-packets:
				if lastSaved.IsZero() || r.Time.Sub(lastSaved) >= interval {
					if err := save(r); err != nil {
						idleTicker.Stop()
						unsubscribe()
						_ = client.Close()
						events <- event{err: fmt.Errorf("saving reading: %w", err)}
						return
					}
					lastSaved = r.Time
					events <- event{reading: &r}
				}
			}
		}
		idleTicker.Stop()
		last := time.Unix(0, lastAdvertisement.Load())
		log.Printf("ESPHome proxy connection closed after %s; last valid sensor packet was %s (%s ago)",
			time.Since(connectedAt).Round(time.Second), last.Format(time.RFC3339), time.Since(last).Round(time.Second))
		unsubscribe()
		_ = client.Close()
		if ctx.Err() != nil {
			return
		}
		if !waitProxyReconnect(ctx, address) {
			return
		}
	}
}

type tickMsg time.Time
type ui struct {
	s                     *store
	events                <-chan event
	points                []reading
	err                   string
	status                string
	width, height, period int
	metric                int
}

var ranges = []struct {
	name     string
	duration time.Duration
}{{"5m", 5 * time.Minute}, {"1h", time.Hour}, {"6h", 6 * time.Hour}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}, {"All", 0}}

func tick() tea.Cmd                    { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }
func waitEvent(c <-chan event) tea.Cmd { return func() tea.Msg { return <-c } }
func (m ui) Init() tea.Cmd {
	if m.events != nil {
		return tea.Batch(tick(), waitEvent(m.events))
	}
	return tick()
}
func (m ui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = v.Width
		m.height = v.Height
	case tea.KeyMsg:
		switch v.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "left", "h":
			m.period = (m.period + len(ranges) - 1) % len(ranges)
		case "right", "l":
			m.period = (m.period + 1) % len(ranges)
		case "1", "2", "3", "4", "5", "6", "7":
			m.period = int(v.String()[0] - '1')
		case "t":
			m.metric = 0
		case "u":
			m.metric = 1
		case "r":
			m.reload()
		}
	case event:
		if v.err != nil {
			m.err = v.err.Error()
			m.status = "Logger stopped"
			m.events = nil
		} else {
			m.status = "Listening"
			m.reload()
		}
		if m.events != nil {
			return m, waitEvent(m.events)
		}
	case tickMsg:
		return m, tick()
	}
	return m, nil
}
func (m *ui) reload() {
	p, e := m.s.load()
	if e != nil {
		m.err = e.Error()
	} else {
		m.points = p
		m.err = ""
	}
}
func chart(points []reading, width, height, metric int, start, end time.Time) string {
	if width < 10 || height < 3 {
		return "Terminal is too small for the chart."
	}
	buckets := make([]float64, width)
	counts := make([]int, width)
	min, max := math.Inf(1), math.Inf(-1)
	for _, p := range points {
		if p.Time.Before(start) || p.Time.After(end) {
			continue
		}
		val := p.Temp
		if metric == 1 {
			val = float64(p.Humidity)
		}
		x := int(p.Time.Sub(start).Seconds() / end.Sub(start).Seconds() * float64(width))
		if x < 0 {
			x = 0
		}
		if x >= width {
			x = width - 1
		}
		buckets[x] += val
		counts[x]++
	}
	for i, n := range counts {
		if n > 0 {
			buckets[i] /= float64(n)
			min = math.Min(min, buckets[i])
			max = math.Max(max, buckets[i])
		}
	}
	if math.IsInf(min, 1) {
		return "No readings in this time range."
	}
	// Whole-unit bounds keep the scale stable when readings fluctuate slightly.
	min = math.Floor(min) - 1
	max = math.Ceil(max) + 1
	grid := make([][]rune, height)
	for y := range grid {
		grid[y] = []rune(strings.Repeat(" ", width))
	}
	for x, n := range counts {
		if n > 0 {
			y := height - 1 - int(math.Round((buckets[x]-min)/(max-min)*float64(height-1)))
			grid[y][x] = '●'
		}
	}
	var b strings.Builder
	for y, row := range grid {
		val := max - float64(y)*(max-min)/float64(height-1)
		fmt.Fprintf(&b, "%6.1f │%s\n", val, string(row))
	}
	fmt.Fprintf(&b, "       └%s\n", strings.Repeat("─", width))
	left := start.Local().Format("02 Jan 15:04")
	right := end.Local().Format("02 Jan 15:04")
	gap := width - len(left) - len(right)
	if gap < 1 {
		gap = 1
	}
	fmt.Fprintf(&b, "        %s%s%s\n", left, strings.Repeat(" ", gap), right)
	return b.String()
}
func (m ui) View() string {
	width := m.width - 10
	if width > 100 {
		width = 100
	}
	height := m.height - 10
	if height > 18 {
		height = 18
	}
	if m.width == 0 {
		width = 70
		height = 10
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  ATC_EA80E1  ·  Temperature logger\n  %s  |  %s\n", m.status, m.s.path)
	if len(m.points) > 0 {
		p := m.points[len(m.points)-1]
		fmt.Fprintf(&b, "  Latest: %.1f°C  ·  %d%% humidity  ·  battery %d%%  ·  %s\n", p.Temp, p.Humidity, p.Battery, p.Time.Local().Format("02 Jan 15:04:05"))
	} else {
		b.WriteString("  Waiting for the first reading.\n")
	}
	if m.err != "" {
		fmt.Fprintf(&b, "  ERROR: %s\n", m.err)
	}
	label := "Temperature °C"
	if m.metric == 1 {
		label = "Humidity %"
	}
	fmt.Fprintf(&b, "\n  %s  |  ", label)
	for i, r := range ranges {
		if i == m.period {
			fmt.Fprintf(&b, "[%s] ", r.name)
		} else {
			fmt.Fprintf(&b, " %s  ", r.name)
		}
	}
	b.WriteString("\n")
	end := time.Now().Truncate(time.Minute).Add(time.Minute)
	start := end.Add(-ranges[m.period].duration)
	if duration := ranges[m.period].duration; duration > 0 && width >= 10 {
		// Anchor columns to fixed time boundaries, so advancing the window
		// does not reshuffle readings between buckets.
		bucket := (duration + time.Duration(width) - 1) / time.Duration(width)
		end = time.Now().Truncate(bucket).Add(bucket)
		start = end.Add(-bucket * time.Duration(width))
	}
	if ranges[m.period].duration == 0 {
		if len(m.points) > 0 {
			start = m.points[0].Time
			for _, p := range m.points {
				if p.Time.Before(start) {
					start = p.Time
				}
			}
		} else {
			start = end.Add(-time.Hour)
		}
		if end.Sub(start) < time.Minute {
			start = end.Add(-time.Minute)
		}
	}
	b.WriteString(chart(m.points, width, height, m.metric, start, end))
	b.WriteString("\n  ←/→ or 1–7: range  ·  t: temperature  ·  u: humidity  ·  r: reload  ·  q: quit\n")
	return b.String()
}
func main() {
	log.SetFlags(log.Ltime)
	interval := flag.Duration("interval", 0, "minimum interval between saved readings")
	path := flag.String("file", "readings.csv", "CSV file for recorded readings")
	noUI := flag.Bool("no-ui", false, "log continuously without a terminal UI")
	monitor := flag.Bool("monitor", false, "continuously print every matching ATC packet without polling or saving")
	view := flag.Bool("view", false, "view existing readings without Bluetooth")
	web := flag.Bool("web", false, "serve the local dashboard and collect into SQLite")
	proxy := flag.String("proxy", "192.168.1.252", "ESPHome Bluetooth proxy host or host:port (default port 6053)")
	proxies := flag.String("proxies", "", "comma-separated ESPHome proxy hosts for web mode; overrides --proxy")
	proxyKey := flag.String("proxy-key", os.Getenv("ESPHOME_API_KEY"), "ESPHome API encryption key (or set ESPHOME_API_KEY)")
	localBLE := flag.Bool("local-ble", false, "use local Bluetooth instead of proxy outside web mode; add local scanning in web mode")
	scan := flag.Bool("scan", false, "list nearby BLE advertisements received through the proxy, then exit")
	scanLocal := flag.Bool("scan-local", false, "list nearby BLE advertisements using the local Bluetooth adapter, then exit")
	scanSeconds := flag.Float64("scan-seconds", 30, "scan duration in seconds for --scan or --scan-local")
	proxyLogs := flag.Bool("proxy-logs", false, "stream ESPHome proxy logs, then exit")
	sensorConfig := flag.Bool("sensor-config", false, "read current ATC sensor settings through the proxy, then exit")
	sensorATC1441Defaults := flag.Bool("sensor-atc1441-defaults", false, "send ATC1441 restore-defaults (DE) and save (DF) through the proxy")
	configScanSeconds := flag.Float64("config-scan-seconds", 30, "maximum time to wait for sensor advertisement with --sensor-config")
	port := flag.Int("port", 9000, "local dashboard port")
	bind := flag.String("bind", "127.0.0.1", "dashboard listen address")
	db := flag.String("db", "readings.sqlite", "SQLite database for dashboard mode")
	flag.Parse()
	if *proxies != "" && !*web {
		fmt.Fprintln(os.Stderr, "--proxies can only be used with --web")
		os.Exit(2)
	}
	proxyHosts, err := parseProxyHosts(*proxies, *proxy)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if (*scan && *scanLocal) || ((*scan || *scanLocal) && (*proxyLogs || *sensorConfig || *sensorATC1441Defaults)) ||
		(*proxyLogs && (*sensorConfig || *sensorATC1441Defaults)) || (*sensorConfig && *sensorATC1441Defaults) {
		fmt.Fprintln(os.Stderr, "--scan, --scan-local, --proxy-logs, --sensor-config, and --sensor-atc1441-defaults are separate modes")
		os.Exit(2)
	}
	if *proxyLogs {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		if err := streamProxyLogs(ctx, *proxy, *proxyKey); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *sensorConfig || *sensorATC1441Defaults {
		if *configScanSeconds <= 0 || math.IsNaN(*configScanSeconds) || math.IsInf(*configScanSeconds, 0) || *configScanSeconds > 300 {
			fmt.Fprintln(os.Stderr, "--config-scan-seconds must be positive and no more than 300")
			os.Exit(2)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		if err := readSensorConfig(ctx, *proxy, *proxyKey, time.Duration(*configScanSeconds*float64(time.Second)), *sensorATC1441Defaults); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *scan {
		if *localBLE {
			fmt.Fprintln(os.Stderr, "--scan uses the ESPHome proxy; omit --local-ble")
			os.Exit(2)
		}
		if *scanSeconds <= 0 || math.IsNaN(*scanSeconds) || math.IsInf(*scanSeconds, 0) || *scanSeconds > float64((24*time.Hour)/time.Second) {
			fmt.Fprintln(os.Stderr, "--scan-seconds must be positive")
			os.Exit(2)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		if err := scanProxy(ctx, *proxy, *proxyKey, time.Duration(*scanSeconds*float64(time.Second))); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *scanLocal {
		if *scanSeconds <= 0 || math.IsNaN(*scanSeconds) || math.IsInf(*scanSeconds, 0) || *scanSeconds > float64((24*time.Hour)/time.Second) {
			fmt.Fprintln(os.Stderr, "--scan-seconds must be positive")
			os.Exit(2)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		if err := scanLocalBLE(ctx, time.Duration(*scanSeconds*float64(time.Second))); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *interval == 0 {
		*interval = 15 * time.Minute
		if *web {
			// Web mode retains every matching BLE broadcast unless the user
			// explicitly supplies --interval to sample less frequently.
			*interval = 0
		}
	}
	if *monitor {
		if *web || *noUI || *view {
			fmt.Fprintln(os.Stderr, "--monitor cannot be combined with --web, --no-ui, or --view")
			os.Exit(2)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
		defer cancel()
		events := make(chan event, 1)
		collector := collectProxy
		if *localBLE {
			collector = nil
		}
		if collector != nil {
			go collector(ctx, *proxy, *proxyKey, 0, nil, events, true, true)
		} else {
			go collectLocal(ctx, 0, nil, events, true)
		}
		log.Printf("Monitoring ATC_EA80E1 packets; press Ctrl-C to stop")
		for {
			select {
			case <-ctx.Done():
				return
			case result := <-events:
				if result.err != nil {
					log.Print(result.err)
					return
				}
			}
		}
	}
	if *web {
		if *interval < 0 || *noUI || *port < 1 || *port > 65535 {
			fmt.Fprintln(os.Stderr, "use a positive interval and a valid port; --web cannot be combined with --no-ui")
			os.Exit(2)
		}
		if err := runDashboard(*db, *path, *bind, *port, *interval, *view, proxyHosts, *proxyKey, *localBLE); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *interval <= 0 || (*noUI && *view) {
		fmt.Fprintln(os.Stderr, "interval must be positive; --no-ui and --view cannot be combined")
		os.Exit(2)
	}
	abs, e := filepath.Abs(*path)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	s := &store{path: abs}
	if _, e = s.load(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	events := make(chan event, 8)
	if !*view {
		if *localBLE {
			go collectLocal(ctx, *interval, s.append, events, false)
		} else {
			go collectProxy(ctx, *proxy, *proxyKey, *interval, s.append, events, false, false)
		}
	}
	if *noUI {
		fmt.Printf("Listening for %s; saving to %s (interval %s)\n", sensorMAC, abs, *interval)
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-events:
				if e.err != nil {
					fmt.Fprintln(os.Stderr, e.err)
					os.Exit(1)
				}
				p := e.reading
				fmt.Printf("%s  %.1f°C  %d%% humidity  battery %d%%  packet #%d\n", p.Time.Local().Format(time.RFC3339), p.Temp, p.Humidity, p.Battery, p.Counter)
			}
		}
	}
	m := ui{s: s, status: "Viewing saved data", period: 3}
	if !*view {
		m.events = events
		if *localBLE {
			m.status = "Scanning for sensor"
		} else {
			m.status = "Connecting to Bluetooth proxy"
		}
	}
	m.reload()
	if _, e = tea.NewProgram(m, tea.WithAltScreen()).Run(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
