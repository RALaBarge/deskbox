#!/usr/bin/env python3
import json
import sys

BYTE_MAP = {"b": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12}
BINARY_MAP = {"kib": 1024, "mib": 1024**2, "gib": 1024**3, "tib": 1024**4}
DIST_MAP = {"m": 1, "meters": 1, "km": 1000, "miles": 1609.344, "feet": 0.3048, "ft": 0.3048, "inches": 0.0254, "in": 0.0254}
WEIGHT_MAP = {"kg": 1, "g": 0.001, "lb": 0.453592, "lbs": 0.453592, "oz": 0.0283495}
TIME_MAP = {"seconds": 1, "s": 1, "minutes": 60, "min": 60, "hours": 3600, "h": 3600, "days": 86400, "d": 86400}


def _temp(value, f, t):
    if f in ("celsius", "c"):
        if t in ("fahrenheit", "f"):
            return value * 9 / 5 + 32
        if t in ("kelvin", "k"):
            return value + 273.15
        return value
    if f in ("fahrenheit", "f"):
        if t in ("celsius", "c"):
            return (value - 32) * 5 / 9
        if t in ("kelvin", "k"):
            return (value - 32) * 5 / 9 + 273.15
        return value
    if f in ("kelvin", "k"):
        if t in ("celsius", "c"):
            return value - 273.15
        if t in ("fahrenheit", "f"):
            return (value - 273.15) * 9 / 5 + 32
        return value
    return None


def _ratio(value, f, t, table):
    if f in table and t in table:
        return value * table[f] / table[t]
    return None


def main():
    try:
        payload = json.load(sys.stdin)
    except json.JSONDecodeError as e:
        print(f"bad input JSON: {e}", file=sys.stderr)
        sys.exit(1)

    value = float(payload.get("value", 0))
    f = str(payload.get("from_unit", "")).lower()
    t = str(payload.get("to_unit", "")).lower()

    result = None
    if f in ("celsius", "c", "fahrenheit", "f", "kelvin", "k"):
        result = _temp(value, f, t)
    if result is None:
        for table in (BYTE_MAP, BINARY_MAP, DIST_MAP, WEIGHT_MAP, TIME_MAP):
            result = _ratio(value, f, t, table)
            if result is not None:
                break

    if result is None:
        print(f"cannot convert {f!r} to {t!r}: unsupported unit pair", file=sys.stderr)
        sys.exit(1)

    json.dump({"result": round(result, 6), "to_unit": t}, sys.stdout)


if __name__ == "__main__":
    main()
