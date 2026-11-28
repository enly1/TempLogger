# ESP32 sensor display handover

## Goal

Show TempLogger's current sensor cards on an ESP32 with a 240 × 320 display. This covers current metrics only; chart and history endpoints are not needed.

## API location

TempLogger serves its dashboard and JSON API from the same HTTP server. The default host port is `9000` (it can be changed with `--port`). Configure the ESP32 with the TempLogger host's LAN name or IP, for example `http://192.168.1.50:9000`. `127.0.0.1` refers to the ESP32 itself and is not appropriate. If using Docker, publish the host port to the container port used by TempLogger.

The API currently has no authentication. Keep it on a trusted LAN; do not expose it directly to the public internet. Plain HTTP is suitable only on a trusted local network.

## Poll endpoint

`GET /api/status`

Send `Accept: application/json`; no request body is needed. Response has `Cache-Control: no-store`. Poll every 5 seconds to match the browser dashboard. Use a bounded timeout and keep the last good values if a request fails.

Illustrative response:

```json
{
  "latest": {"at": 1795700000000, "deviceId": "A4:C1:38:12:34:56", "format": "ATC", "temperature": 21.4, "humidity": 48.2, "battery": 87, "millivolts": 2960, "rssi": -64},
  "latestSensors": [
    {"at": 1795700000000, "deviceId": "A4:C1:38:12:34:56", "format": "ATC", "temperature": 21.4, "humidity": 48.2, "battery": 87, "millivolts": 2960, "rssi": -64}
  ],
  "first": 1795600000000,
  "interval": 30000,
  "state": "Collecting",
  "now": 1795700001000
}
```

`latestSensors` is the array to use for the individual sensor cards. It contains each sensor's newest stored reading, sorted by `deviceId`. Fields are:

- `deviceId`: stable sensor identifier, commonly a MAC address.
- `format`: decoder/protocol label, for example `ATC`, `PVVX`, `BTHome`, or `MiBeacon`.
- `at`: Unix epoch timestamp in **milliseconds**.
- `temperature`: degrees Celsius; number or `null`.
- `humidity`: relative humidity percent; number or `null`.
- `battery`: battery percent; number or `null`.
- `millivolts`: battery voltage in millivolts; number or `null`.
- `rssi`: received signal strength in dBm, usually a negative integer.

Treat `null` metrics as unavailable and display an em dash. Match the website's formatting: temperature to one decimal plus `°C`, humidity to one decimal plus `%`, battery as whole percent, voltage as volts to three decimals (`millivolts / 1000`, `V`), RSSI as an integer plus `dBm`.

`latest` is one overall latest reading, retained as a compatibility fallback. The website displays `latestSensors`; if that list is empty, it falls back to `latest` as one generic sensor card. If both are empty/null, show a waiting/no-readings state.

Other response fields: `state` describes collector status (for example `Collecting`); `interval` is the configured collection interval in milliseconds; `now` and `first` are epoch milliseconds. `first` is not needed for current cards. The website treats readings as stale when `state` is `Collecting` and `now - latest.at` exceeds max(90 seconds, three `interval`s). The ESP32 can use this rule for a stale indicator.

## Optional aliases

For friendly names, optionally call `GET /api/sensors`, which returns entries like:

```json
[{"deviceId":"A4:C1:38:12:34:56","alias":"Kitchen","format":"ATC"}]
```

Join on `deviceId`; show non-empty `alias` instead of the ID. This endpoint is not required for a read-only display. The website's alias-edit endpoint is `PUT /api/sensors/{deviceId}/alias`; it is unnecessary for the ESP32 display.

## 240 × 320 layout guidance

Use portrait orientation. For one sensor, show a compact card with:

1. Header: alias or shortened device ID, format, and freshness/update time.
2. Temperature as the largest value (for example `21.4 °C`).
3. A two-column grid for humidity, battery, voltage, and signal.
4. A small footer such as `Updated 14:32` or `No data`.

Use roughly 8–12 px side margins and avoid long unbroken MAC addresses. For several sensors, stack compact cards vertically and scroll; prioritize temperature, humidity, battery, and update time, with voltage/RSSI in smaller text or a detail view. Convert `at` from milliseconds to local time using the ESP32's configured timezone. If its clock is not synchronized, show relative age (such as `12s ago`) or omit the clock time.

## Refresh and failure behavior

- Reuse the Wi-Fi connection and poll every 5 seconds. After failures, back off (for example 5, 10, 20 seconds, capped at 60 seconds).
- On timeout, non-2xx status, or malformed JSON, keep the last displayed values and mark the data offline/stale; never replace missing values with zero.
- When there is no data, show `Waiting for sensor data`.
- A new sensor appearing in `latestSensors` should add a card. Each returned sensor is represented by its most recent stored row.

Do not call `/api/readings` or `/api/sensor-readings`: those are time-series endpoints used by charts and require `from`/`to` timestamps. `/api/status` supplies everything needed for current sensor cards.
