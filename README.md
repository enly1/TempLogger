# Bluetooth temperature logger

## Docker and Arcane deployment

The root `compose.yaml` is ready to deploy from Arcane. It pulls the private
`enly1/templogger:latest` image for `linux/amd64`, publishes the dashboard on
host port `9002` to container port `9000`, and stores SQLite and CSV data in the
named `templogger_data` volume. The default ESPHome proxy is `192.168.1.252:6053`.

Before deploying in Arcane:

1. In **Images → Registries**, add Docker Hub credentials for `docker.io` so
   Arcane can pull the private image. Use a Docker Hub access token.
2. If the ESPHome proxy uses API encryption, add `ESPHOME_API_KEY` under
   **Projects → Variables**, mark it as a secret, and scope it to the agent
   that will run TempLogger. The Compose file passes this value to the app.
3. Create a project using this `compose.yaml` and deploy it. Arcane's pull
   policy is set to **always** in the Compose file so redeploys pick up the
   current `latest` image.

The dashboard has no authentication; limit host port 9002 to trusted networks.
The volume persists across redeploys. Container deployments start with an empty
database; to retain existing history, create a consistent SQLite backup using
`sqlite3 readings.sqlite '.backup backup.sqlite'` and copy it into the volume
as `/data/readings.sqlite` before starting the logger. Ensure that UID 10001
can read and write the database and its containing directory.

For a local image build:

```sh
docker buildx build --platform linux/amd64 -t templogger:local --load .
```

The Arcane Compose file uses the published Docker Hub image and does not build
from source.

## Live web dashboard

The Go app connects to the ESPHome Bluetooth proxy at `192.168.1.252:6053`, subscribes to its BLE advertisement stream, decodes **ATC_EA80E1** broadcasts, and saves readings to SQLite. The proxy connection uses ESPHome's native API; if API encryption is enabled, set `ESPHOME_API_KEY` to the proxy's API encryption key. Run it from Terminal:

```sh
cd /path/to/TempLogger
go build -o templogger .
./templogger --web
```

Open [the local dashboard](http://127.0.0.1:9000). It shows temperature, humidity, battery and the latest reading time. Select a range from **5m** through **45m**, or choose **1h**, **6h**, **24h**, **7d**, **30d**, or **All**. Use the arrows to move through history and hover over a point for the recorded values. **Live** returns to the latest window. Wide ranges display a subset of recorded points, preserving extremes. Zoom in to see individual readings.

TempLogger requests passive scanning from proxies that support remote scan-mode control. Otherwise, set the ESPHome BLE tracker scan parameters to passive in the proxy configuration. Web mode prints every matching sensor broadcast but saves only the first packet for each ATC counter by default. Set `--interval 1m` or `--interval 15m` to save at most one unique reading per interval; this does not change the sensor's broadcast cadence. Collection continues while the browser is closed as long as the Go process and proxy are running. Press **Ctrl-C** to stop the process. `--proxy HOST[:PORT]` changes the proxy address, `--proxy-key KEY` supplies its API encryption key, and `--local-ble` uses the computer's Bluetooth adapter instead.

Use `./templogger --scan --scan-seconds 30` to list advertisements received through the proxy, including raw packets and any decoded ATC reading. Use `./templogger --proxy-logs` to stream the proxy's ESPHome logs remotely. These modes help confirm whether the proxy is hearing the sensor and whether the API subscription is active.

Use `./templogger --sensor-config` to find ATC_EA80E1 in the proxy's passive advertisement stream, briefly connect over GATT, and print the sensor's current beacon and measurement intervals. This command only requests the config block; it does not change settings. The sensor must be connectable and the proxy must have a free GATT connection slot. `--config-scan-seconds 60` increases the time allowed to find its advertisement.

The default database is `readings.sqlite`. Startup imports the selected sensor’s existing rows from `readings.csv` and skips timestamps already present in SQLite. Use `--db` to choose the database path, `--file` to choose the CSV import source, or `--web --view` to browse history without Bluetooth. The dashboard is served on loopback by default. Use `--bind` to change the listening address and `--port` to change the port.

## Go logger and terminal chart (CSV mode)

The Go app listens for ATC broadcasts from **ATC_EA80E1** (MAC `A4:C1:38:EA:80:E1`) through the ESPHome Bluetooth proxy by default. It matches the MAC inside the broadcast and saves observations to `readings.csv`, compatible with the previous Python logger. Add `--local-ble` to listen with this computer's Bluetooth adapter instead.

```sh
cd /path/to/TempLogger
go build -o templogger .
./templogger
```

The app records the first received broadcast and then listens for the next at least 15 minutes later. Press **1–7** or **left/right** for 5 minutes, 1 hour, 6 hours, 24 hours, 7 days, 30 days, or all available data; **t** selects temperature, **u** selects humidity, and **q** quits. Each chart column is the average of readings in that time bucket; blank columns represent periods without readings. Fixed ranges use time-aligned buckets, so advancing the window does not redistribute past readings. A new reading can change the average in its own bucket. The vertical scale uses whole-unit boundaries; resizing, changing ranges, or a change in the overall range may reposition points. The All view expands as new data arrives. Times are displayed in the computer’s local time and stored in UTC.

```sh
./templogger --interval 1m              # sample approximately every minute
./templogger --monitor                  # print every matching packet continuously; do not save
./templogger --view                     # view saved data, without Bluetooth
./templogger --no-ui                    # collect in the terminal without the chart
./templogger --file /path/to/log.csv    # choose a different CSV file
```

For multiple readings in the five-minute view, use a shorter collection interval, for example `--interval 30s`.

The logger and proxy must stay running; no measurements are invented for gaps. Use only one logging process per CSV file. The `--view` mode loads the file on startup; press **r** to reload data written by a separate logger. The application uses the sensor’s ATC 0x181A broadcast format; changing the device to a different broadcast format requires updating the logger. Go and network access to the ESPHome API are required to build and collect.

## Earlier Python scripts

The sensor in the photo looks like a Xiaomi LYWSD03MMC, but its installed firmware and broadcast settings cannot be identified from its appearance. This listens for unencrypted ATC, PVVX custom and BTHome v2 advertisements; it does not connect to the device or change its settings. Other formats, including encrypted advertisements and Xiaomi/Mi format, are not decoded.

## Setup

Requires Python 3, Bluetooth LE and a machine within range. On macOS, enable Bluetooth and allow the terminal application Bluetooth access if prompted (System Settings → Privacy & Security → Bluetooth). On Linux, Bluetooth and BlueZ must be available.

```sh
cd /path/to/TempLogger
python3 -m venv .venv
source .venv/bin/activate
python -m pip install -r requirements.txt
```

## Find the sensor

```sh
python templogger.py scan --seconds 60
```

Compare decoded temperature and humidity with the screen. A scan lists other nearby Bluetooth devices too; service data and device identifiers can be sensitive, so review output before sharing. The ID is usually a macOS-generated UUID on a Mac and a MAC address on Linux. Use the ID from the machine that will run the logger. A scan identifies the broadcast format, not necessarily the exact firmware or version. If there are multiple similar devices, move the sensor close to the computer or change its temperature slightly to identify it.

If no reading appears, try a longer scan and check Bluetooth permissions and range. Devices may broadcast infrequently. If the sensor appears but is marked unsupported, encrypted, or has no measurement, retain its name, ID and service data for troubleshooting; the firmware may use another format, encryption, or expose data only through a connection. Some devices send separate advertisements with and without service data.

## Log

Replace the example with the exact ID shown by the scan:

```sh
python templogger.py log --device 'YOUR-DEVICE-ID' --interval 60 --output readings.csv
```

The logger listens continuously and appends the first received reading, then the next received reading at least 60 seconds after the last logged one. This is an observation interval, not a request for a fresh measurement: the sensor controls its own measurement and broadcast intervals. Timestamps are UTC. Existing CSV files are appended to; stop with Ctrl-C. No rows are fabricated while the sensor is out of range, and the machine must remain awake and running for logging to continue.

Formats: [PVVX firmware](https://github.com/pvvx/ATC_MiThermometer), [ATC firmware](https://github.com/atc1441/ATC_MiThermometer), [BTHome v2](https://bthome.io/format/).

## Xiaomi-compatible connection

If the device broadcasts Xiaomi `fe95` data but exposes the LYWSD03MMC notification characteristic, use this alternative (confirmed working with both sensors found on this Mac):

```sh
python connected_logger.py --device 'YOUR-DEVICE-ID' --interval 60 --output connected_readings.csv
```

This maintains a Bluetooth connection, records notifications no more often than the requested interval, and retries after a disconnect. Battery is reported in millivolts. The device controls notification timing, and the connection may use more battery than passive broadcasting or prevent another client from connecting. Press Ctrl-C to stop. This method does not establish which firmware is installed.
