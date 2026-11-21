#!/usr/bin/env python3
"""Listen for common custom-firmware BLE thermometer advertisements."""
import argparse
import asyncio
import csv
from datetime import datetime, timezone
from pathlib import Path
import sys
import time

UUID_181A = '0000181a-0000-1000-8000-00805f9b34fb'
UUID_BTHOME = '0000fcd2-0000-1000-8000-00805f9b34fb'
FIELDS = ['timestamp_utc', 'device_id', 'name', 'format', 'temperature_c', 'humidity_percent', 'battery_percent', 'battery_mv', 'rssi_dbm']


def decode(service_data):
    """Return supported readings, or None. Bleak service data excludes UUID."""
    data = {k.lower(): bytes(v) for k, v in service_data.items()}
    p = data.get(UUID_181A, b'')
    if len(p) == 13:
        return dict(format='ATC', temperature_c=int.from_bytes(p[6:8], 'big', signed=True)/10,
                    humidity_percent=p[8], battery_percent=p[9], battery_mv=int.from_bytes(p[10:12], 'big'))
    if len(p) == 15:
        return dict(format='PVVX', temperature_c=int.from_bytes(p[6:8], 'little', signed=True)/100,
                    humidity_percent=int.from_bytes(p[8:10], 'little')/100,
                    battery_mv=int.from_bytes(p[10:12], 'little'), battery_percent=p[12])
    p = data.get(UUID_BTHOME, b'')
    if not p or p[0] >> 5 != 2 or p[0] & 1:
        return None
    result = dict(format='BTHome v2')
    # Stop at unknown fields: their lengths cannot safely be inferred.
    specs = {0: ('packet_id', 1, False, 1), 1: ('battery_percent', 1, False, 1),
             2: ('temperature_c', 2, True, 100), 3: ('humidity_percent', 2, False, 100),
             0x2e: ('humidity_percent', 1, False, 1), 0x0c: ('battery_mv', 2, False, 1)}
    i = 1
    while i < len(p):
        spec = specs.get(p[i]); i += 1
        if spec is None:
            break
        name, length, signed, scale = spec
        if i + length > len(p):
            return None
        result[name] = int.from_bytes(p[i:i+length], 'little', signed=signed) / scale
        i += length
    return result if 'temperature_c' in result or 'humidity_percent' in result else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    scan = sub.add_parser('scan', help='show nearby Bluetooth advertisements and decoded readings')
    scan.add_argument('--seconds', type=float, default=30)
    log = sub.add_parser('log', help='log one chosen sensor to CSV until Ctrl-C')
    log.add_argument('--device', required=True, help='device ID shown by scan (UUID on macOS; MAC on Linux)')
    log.add_argument('--interval', type=float, default=60, help='minimum seconds between logged observations (default: 60)')
    log.add_argument('--output', default='readings.csv')
    args = parser.parse_args()
    if (args.command == 'scan' and args.seconds <= 0) or (args.command == 'log' and args.interval <= 0):
        parser.error('duration and interval must be positive')
    try:
        from bleak import BleakScanner
    except ImportError:
        sys.exit('Bleak is missing. Follow the installation instructions in README.md.')

    async def run():
        seen = set()
        last = None
        out = None
        if args.command == 'log':
            path = Path(args.output).expanduser()
            out = path.open('a', newline='', encoding='utf-8')
            writer = csv.DictWriter(out, fieldnames=FIELDS, extrasaction='ignore')
            if path.stat().st_size == 0:
                writer.writeheader(); out.flush()
            print(f'Listening for {args.device}; appending to {path}. Press Ctrl-C to stop.', flush=True)
        else:
            print(f'Scanning for {args.seconds:g} seconds; match a reading to your sensor display.', flush=True)

        def detected(device, advertisement):
            nonlocal last
            ident = device.address
            values = decode(advertisement.service_data)
            name = advertisement.local_name or device.name or ''
            if args.command == 'scan':
                # Report changed payloads, but avoid flooding on repeats.
                key = (ident, tuple(sorted((k, bytes(v)) for k, v in advertisement.service_data.items())), name)
                if key in seen:
                    return
                seen.add(key)
                raw = ', '.join(f'{k}={bytes(v).hex()}' for k, v in advertisement.service_data.items()) or '(none)'
                print(f'\n{name or "(unnamed)"}  ID: {ident}  RSSI: {advertisement.rssi} dBm\n  service data: {raw}\n  decoded: {values or "unsupported, encrypted, or no measurement"}', flush=True)
                return
            if ident.casefold() != args.device.casefold() or values is None:
                return
            now = time.monotonic()
            if last is not None and now - last < args.interval:
                return
            row = dict(timestamp_utc=datetime.now(timezone.utc).isoformat(timespec='seconds'), device_id=ident,
                       name=name, rssi_dbm=advertisement.rssi, **values)
            writer.writerow(row); out.flush(); last = now
            print(f"{row['timestamp_utc']}  {values.get('temperature_c', '-')} °C  {values.get('humidity_percent', '-')} %  battery {values.get('battery_percent', '-')} %", flush=True)
        try:
            async with BleakScanner(detection_callback=detected):
                if args.command == 'scan':
                    await asyncio.sleep(args.seconds)
                else:
                    await asyncio.Event().wait()
        finally:
            if out:
                out.close()
    try:
        asyncio.run(run())
    except KeyboardInterrupt:
        print('\nStopped.')
    except Exception as exc:
        sys.exit(f'Bluetooth or file error: {exc}')

if __name__ == '__main__':
    main()
