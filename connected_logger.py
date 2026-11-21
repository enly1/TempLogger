#!/usr/bin/env python3
"""Log LYWSD03MMC readings using its Bluetooth notification characteristic."""
import argparse
import asyncio
import csv
from datetime import datetime, timezone
from pathlib import Path
import time
from bleak import BleakClient

CHARACTERISTIC = 'ebe0ccc1-7a0a-4b0c-8a1a-6ff2997da3a6'
FIELDS = ['timestamp_utc', 'device_id', 'temperature_c', 'humidity_percent', 'battery_mv']


def decode(data):
    if len(data) < 5:
        return None
    return {'temperature_c': int.from_bytes(data[:2], 'little', signed=True) / 100,
            'humidity_percent': data[2], 'battery_mv': int.from_bytes(data[3:5], 'little')}


async def run(args):
    path = Path(args.output).expanduser()
    with path.open('a', newline='', encoding='utf-8') as out:
        writer = csv.DictWriter(out, fieldnames=FIELDS)
        if path.stat().st_size == 0:
            writer.writeheader(); out.flush()
        last = None
        while True:
            disconnected = asyncio.Event()
            try:
                print(f'Connecting to {args.device}...', flush=True)
                async with BleakClient(args.device, timeout=30, disconnected_callback=lambda _: disconnected.set()) as client:
                    def reading(_, data):
                        nonlocal last
                        values = decode(data)
                        now = time.monotonic()
                        if values is None or (last is not None and now - last < args.interval):
                            return
                        row = {'timestamp_utc': datetime.now(timezone.utc).isoformat(timespec='seconds'),
                               'device_id': args.device, **values}
                        writer.writerow(row); out.flush(); last = now
                        print(f"{row['timestamp_utc']}  {values['temperature_c']:.2f} °C  {values['humidity_percent']} %  {values['battery_mv']} mV", flush=True)
                    await client.start_notify(CHARACTERISTIC, reading)
                    print('Listening; press Ctrl-C to stop.', flush=True)
                    await disconnected.wait()
                print('Disconnected; reconnecting in 10 seconds.', flush=True)
            except Exception as exc:
                print(f'Connection error: {exc}; retrying in 10 seconds.', flush=True)
            await asyncio.sleep(10)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--device', required=True, help='ID from the scan on this computer')
    p.add_argument('--interval', type=float, default=60, help='minimum seconds between logged notifications')
    p.add_argument('--output', default='connected_readings.csv')
    args = p.parse_args()
    if args.interval <= 0:
        p.error('--interval must be positive')
    try:
        asyncio.run(run(args))
    except KeyboardInterrupt:
        print('\nStopped.')

if __name__ == '__main__':
    main()
