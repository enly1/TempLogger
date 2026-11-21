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
	Name                                string
	Temp                                float64
	Humidity, Battery, Millivolts, RSSI int
	Counter                             uint8
}

func decodeATC(p []byte, rssi int) (reading, bool) {
	return decodeATCNamed(p, rssi, "")
}

func decodeATCNamed(p []byte, rssi int, name string) (reading, bool) {
	if len(p) != 13 || fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", p[0], p[1], p[2], p[3], p[4], p[5]) != sensorMAC {
		return reading{}, false
	}
	return reading{Time: time.Now().UTC(), Name: name, Temp: float64(int16(binary.BigEndian.Uint16(p[6:8]))) / 10, Humidity: int(p[8]), Battery: int(p[9]), Millivolts: int(binary.BigEndian.Uint16(p[10:12])), RSSI: rssi, Counter: p[12]}, true
}

// decodeProxyAdvertisement extracts the 0x181A service data and local name
// from the raw BLE advertising structures sent by an ESPHome proxy.
func decodeProxyAdvertisement(data []byte, rssi int) (reading, bool) {
	var name string
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
		case 0x08, 0x09: // shortened or complete local name
			name = string(field)
		case 0x16: // 16-bit UUID service data
			if len(field) >= 2 && field[0] == 0x1a && field[1] == 0x18 {
				serviceData = field[2:]
			}
		}
		data = data[fieldLen+1:]
	}
	return decodeATCNamed(serviceData, rssi, name)
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
	e = w.Write([]string{r.Time.UTC().Format(time.RFC3339Nano), sensorMAC, "ATC_EA80E1", "ATC", strconv.FormatFloat(r.Temp, 'f', 1, 64), strconv.Itoa(r.Humidity), strconv.Itoa(r.Battery), strconv.Itoa(r.Millivolts), strconv.Itoa(r.RSSI)})
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
				broadcastName := result.LocalName()
				for _, data := range result.ServiceData() {
					if !strings.EqualFold(data.UUID.String(), "0000181a-0000-1000-8000-00805f9b34fb") {
						continue
					}
					r, ok := decodeATCNamed(data.Data, int(result.RSSI), broadcastName)
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
			return decodeATCNamed(service.Data, int(adv.Rssi), string(adv.Name))
		}
	}
	return reading{}, false
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

	var count int
	seen := make(map[string]struct{})
	var scanMu sync.Mutex
	printAdvertisement := func(key string, line string) {
		scanMu.Lock()
		defer scanMu.Unlock()
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		count++
		if count <= 150 {
			fmt.Println(line)
		}
	}
	unsubscribe, err := subscribeProxyAdvertisements(client, func(msg proto.Message) {
		switch batch := msg.(type) {
		case *pb.BluetoothLERawAdvertisementsResponse:
			for _, adv := range batch.Advertisements {
				data := adv.Data
				key := fmt.Sprintf("%012X:%x", adv.Address, data)
				line := fmt.Sprintf("%012X  RSSI %d dBm  AD %x", adv.Address, adv.Rssi, data)
				if r, ok := decodeProxyAdvertisement(data, int(adv.Rssi)); ok {
					line += fmt.Sprintf("  => ATC %.1f°C %d%% RH battery %d%%", r.Temp, r.Humidity, r.Battery)
				}
				printAdvertisement(key, line)
			}
		case *pb.BluetoothLEAdvertisementResponse:
			key := fmt.Sprintf("%012X:%x:%s", batch.Address, batch.Name, batch.String())
			line := fmt.Sprintf("%012X  RSSI %d dBm  name %q  service data:", batch.Address, batch.Rssi, batch.Name)
			for _, service := range batch.ServiceData {
				line += fmt.Sprintf(" %s=%x", service.Uuid, service.Data)
			}
			if r, ok := decodeLegacyProxyAdvertisement(batch); ok {
				line += fmt.Sprintf("  => ATC %.1f°C %d%% RH battery %d%%", r.Temp, r.Humidity, r.Battery)
			}
			printAdvertisement(key, line)
		}
	})
	if err != nil {
		return err
	}
	defer unsubscribe()
	fmt.Printf("Scanning BLE advertisements through %s for %s...\n", address, duration)
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-client.Done():
		return errors.New("proxy disconnected during scan")
	case <-timer.C:
	}
	scanMu.Lock()
	defer scanMu.Unlock()
	if count > 150 {
		fmt.Printf("... (%d additional distinct advertisements omitted)\n", count-150)
	}
	fmt.Printf("Scan complete: %d distinct advertisements received.\n", count)
	if count == 0 {
		fmt.Println("No BLE advertisements received; check proxy scan status and sensor range.")
	}
	return nil
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

func streamProxyLogs(ctx context.Context, host, encryptionKey string) error {
	address := proxyAddress(host)
	opts := []esphome.Option{esphome.WithClientInfo("TempLogger proxy log viewer")}
	if encryptionKey != "" {
		opts = append(opts, esphome.WithEncryptionKey(encryptionKey))
	}
	client, err := esphome.DialWithContext(ctx, address, 5*time.Second, opts...)
	if err != nil {
		return fmt.Errorf("connecting to ESPHome proxy %s: %w", address, err)
	}
	defer client.Close()
	fmt.Printf("Connected to ESPHome proxy %s; subscribing to verbose device logs.\n", address)
	unsubscribe, err := client.SubscribeLogs(pb.LogLevel_LOG_LEVEL_VERBOSE, func(msg *pb.SubscribeLogsResponse) {
		fmt.Printf("[%s] %s", msg.Level, string(msg.Message))
		if len(msg.Message) == 0 || msg.Message[len(msg.Message)-1] != '\n' {
			fmt.Println()
		}
	})
	if err != nil {
		return fmt.Errorf("subscribing to proxy logs: %w", err)
	}
	defer unsubscribe()
	fmt.Printf("Streaming ESPHome logs from %s; press Ctrl-C to stop.\n", address)
	select {
	case <-ctx.Done():
		return nil
	case <-client.Done():
		return errors.New("proxy disconnected while streaming logs")
	}
}

func collectProxy(ctx context.Context, host, encryptionKey string, interval time.Duration, save func(reading) error, events chan<- event, monitor, logPackets bool) {
	address := proxyAddress(host)
	packets := make(chan reading, 128)
	lastSaved := time.Time{}
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
			events <- event{err: fmt.Errorf("ESPHome Bluetooth proxy %s: %w (check that TCP 6053 is reachable and the API encryption key is correct)", address, err)}
			return
		}

		log.Printf("Connected to ESPHome Bluetooth proxy %s", address)
		connectedAt := time.Now()
		var lastAdvertisement atomic.Int64
		lastAdvertisement.Store(connectedAt.UnixNano())
		requestPassiveProxyScan(client)
		unsubscribe, err := subscribeProxyAdvertisements(client, func(msg proto.Message) {
			handle := func(r reading, ok bool) {
				if !ok {
					return
				}
				lastAdvertisement.Store(r.Time.UnixNano())
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
		})
		if err != nil {
			_ = client.Close()
			events <- event{err: fmt.Errorf("subscribing to proxy advertisements: %w", err)}
			return
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
					log.Printf("BLE stream is connected but no valid ATC_EA80E1 packet has arrived for %s (last packet %s)", idle, last.Format(time.RFC3339))
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
		log.Printf("Proxy connection lost; reconnecting to %s in 5 seconds", address)
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		log.Printf("Attempting ESPHome proxy reconnect to %s", address)
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
	proxyKey := flag.String("proxy-key", os.Getenv("ESPHOME_API_KEY"), "ESPHome API encryption key (or set ESPHOME_API_KEY)")
	localBLE := flag.Bool("local-ble", false, "scan with this computer's Bluetooth adapter instead of the proxy")
	scan := flag.Bool("scan", false, "list nearby BLE advertisements received through the proxy, then exit")
	scanSeconds := flag.Float64("scan-seconds", 30, "scan duration in seconds for --scan")
	proxyLogs := flag.Bool("proxy-logs", false, "stream ESPHome proxy logs, then exit")
	sensorConfig := flag.Bool("sensor-config", false, "read current ATC sensor settings through the proxy, then exit")
	sensorATC1441Defaults := flag.Bool("sensor-atc1441-defaults", false, "send ATC1441 restore-defaults (DE) and save (DF) through the proxy")
	configScanSeconds := flag.Float64("config-scan-seconds", 30, "maximum time to wait for sensor advertisement with --sensor-config")
	port := flag.Int("port", 9000, "local dashboard port")
	bind := flag.String("bind", "127.0.0.1", "dashboard listen address")
	db := flag.String("db", "readings.sqlite", "SQLite database for dashboard mode")
	flag.Parse()
	if (*scan && *proxyLogs) || ((*sensorConfig || *sensorATC1441Defaults) && (*scan || *proxyLogs)) || (*sensorConfig && *sensorATC1441Defaults) {
		fmt.Fprintln(os.Stderr, "--scan, --proxy-logs, --sensor-config, and --sensor-atc1441-defaults are separate modes")
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
		if err := runDashboard(*db, *path, *bind, *port, *interval, *view, *proxy, *proxyKey, *localBLE); err != nil {
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
