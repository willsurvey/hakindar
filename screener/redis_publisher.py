#!/usr/bin/env python3
"""
Redis Publisher Module — Bridge antara screener output dan Go trading system.

Modul ini di-import oleh screener.py ATAU dipanggil setelah screener selesai.
Fungsi utama: publish hasil screening ke Redis agar Go system bisa membaca
watchlist, IHSG status, dan entry plan secara real-time.

Redis Key Convention:
    watchlist:<date>:<ticker>  = JSON entry plan per saham
    watchlist:<date>:tickers   = "BBRI,TLKM,ADRO" (comma-separated)
    watchlist:date             = "2026-04-18"
    watchlist:count            = "3"
    ihsg:safe                  = "1" atau "0"
    ihsg:trend                 = "BULLISH" / "BEARISH" / "NEUTRAL" / "UNKNOWN"
"""

import json
import logging
import os
from datetime import datetime
from typing import Dict, List, Optional

log = logging.getLogger(__name__)

# TTL untuk data watchlist di Redis (24 jam — expired otomatis besok)
WATCHLIST_TTL_SECONDS = 86400


def _get_redis_client():
    """Create Redis connection from environment variables."""
    try:
        import redis
    except ImportError:
        log.warning("redis package not installed — skipping Redis publish")
        return None

    host = os.environ.get("REDIS_HOST", "localhost")
    port = int(os.environ.get("REDIS_PORT", "6379"))
    password = os.environ.get("REDIS_PASSWORD", "")

    try:
        client = redis.Redis(
            host=host,
            port=port,
            password=password if password else None,
            decode_responses=True,
            socket_timeout=5,
        )
        client.ping()
        log.info(f"✅ Redis connected: {host}:{port}")
        return client
    except Exception as e:
        log.warning(f"⚠️ Redis connection failed: {e}")
        return None


def publish_screening_to_redis(
    results: List[Dict],
    market_ctx: Dict,
    date_str: Optional[str] = None,
):
    """
    Publish screening results to Redis for Go trading system consumption.

    Args:
        results: List of screener output dicts (each has ticker, entry_1, etc.)
        market_ctx: IHSG market context dict
        date_str: Override date string (default: today)
    """
    client = _get_redis_client()
    if client is None:
        return

    if date_str is None:
        date_str = datetime.now().strftime("%Y-%m-%d")

    pipe = client.pipeline()

    try:
        # 1. Publish IHSG safety status
        market_safe = market_ctx.get("market_safe", True)
        ihsg_trend = market_ctx.get("ihsg_trend", "UNKNOWN")

        pipe.set("ihsg:safe", "1" if market_safe else "0", ex=WATCHLIST_TTL_SECONDS)
        pipe.set("ihsg:trend", ihsg_trend, ex=WATCHLIST_TTL_SECONDS)
        pipe.set("ihsg:close", str(market_ctx.get("ihsg_close", 0)), ex=WATCHLIST_TTL_SECONDS)
        pipe.set("ihsg:change_pct", str(market_ctx.get("ihsg_change_pct", 0)), ex=WATCHLIST_TTL_SECONDS)

        log.info(f"📡 IHSG → Redis: safe={market_safe}, trend={ihsg_trend}")

        # 2. Publish watchlist metadata
        pipe.set("watchlist:date", date_str, ex=WATCHLIST_TTL_SECONDS)
        pipe.set("watchlist:count", str(len(results)), ex=WATCHLIST_TTL_SECONDS)

        # 3. Publish individual entry plans
        tickers = []
        for item in results:
            ticker = item.get("ticker", "")
            if not ticker:
                continue

            tickers.append(ticker)

            # Build entry plan JSON
            entry_plan = {
                "ticker": ticker,
                "entry_1": item.get("entry_1", 0),
                "entry_2": item.get("entry_2", 0),
                "stop_loss": item.get("stop_loss", 0),
                "target_1": item.get("target_1", item.get("tp_1", 0)),
                "target_2": item.get("target_2", item.get("tp_2", 0)),
                "confidence": item.get("final_score", item.get("score", 0)),
                "reason": item.get("reason", item.get("signals_positive", "")),
                "strategy": item.get("strategy", "INTRADAY"),
            }

            key = f"watchlist:{date_str}:{ticker}"
            pipe.set(key, json.dumps(entry_plan, ensure_ascii=False), ex=WATCHLIST_TTL_SECONDS)

        # 4. Publish ticker list for Go to iterate
        pipe.set(
            f"watchlist:{date_str}:tickers",
            ",".join(tickers),
            ex=WATCHLIST_TTL_SECONDS,
        )

        # Execute pipeline
        pipe.execute()
        log.info(f"✅ Watchlist published to Redis: {len(tickers)} stocks [{', '.join(tickers)}]")

    except Exception as e:
        log.error(f"❌ Redis publish failed: {e}")
    finally:
        try:
            client.close()
        except Exception:
            pass


def publish_running_trade(summary: Dict):
    """
    Publish ringkasan running trade ke Redis untuk konsumsi AI analyst Go.

    Key: stockbit:running_trade
    TTL: 1 jam

    Format summary:
    {
      "timestamp": "16:14:51",
      "last_trade_number": 2330610,
      "summary": {
        "CUAN": {"net_lot": 4403, "buy_lot": 4403, "sell_lot": 0,
                 "dominant_buyer": "MG", "foreign_net": -614}
      }
    }
    """
    client = _get_redis_client()
    if client is None:
        return

    try:
        client.set(
            "stockbit:running_trade",
            json.dumps(summary, ensure_ascii=False),
            ex=3600,  # TTL 1 jam
        )
        n = len(summary.get("summary", {}))
        log.info(f"📡 Running trade → Redis: {n} simbol teraktif")
    except Exception as e:
        log.warning(f"⚠️ Running trade publish failed: {e}")
    finally:
        try:
            client.close()
        except Exception:
            pass


def publish_keystats(symbol: str, keystats: Dict):
    """
    Publish keystats fundamental per simbol ke Redis untuk AI analyst.

    Key: stockbit:keystats:{symbol}
    TTL: 24 jam (data fundamental tidak berubah harian)

    Format keystats:
    {
      "symbol": "BBRI",
      "pe_ttm": 9.18,
      "eps_ttm": 373.80,
      "roe_ttm": 17.48,
      "revenue_growth": 18.71,
      "net_profit_margin": 30.18,
      "dividend_yield": 10.09,
      "piotroski_score": 5.0,
      "52w_high": 4450,
      "52w_low": 3220,
      "price_return_ytd": -6.28
    }
    """
    client = _get_redis_client()
    if client is None:
        return

    try:
        keystats["symbol"] = symbol
        client.set(
            f"stockbit:keystats:{symbol}",
            json.dumps(keystats, ensure_ascii=False),
            ex=86400,  # TTL 24 jam
        )
        log.debug(f"📡 Keystats {symbol} → Redis (PE={keystats.get('pe_ttm','?')}, ROE={keystats.get('roe_ttm','?')}%)")
    except Exception as e:
        log.warning(f"⚠️ Keystats publish failed for {symbol}: {e}")
    finally:
        try:
            client.close()
        except Exception:
            pass


def publish_from_json_file(json_path: str):
    """
    Load screening output JSON file and publish to Redis.
    Useful for manual/CLI usage: python redis_publisher.py combined_screening.json
    """
    try:
        with open(json_path, "r", encoding="utf-8") as f:
            data = json.load(f)
    except Exception as e:
        log.error(f"Failed to read {json_path}: {e}")
        return

    # Extract results and market context from combined output format
    results = data.get("logika_lama_intraday", data.get("data", []))
    market_ctx = data.get("market_context", {})
    date_str = data.get("date", datetime.now().strftime("%Y-%m-%d"))

    publish_screening_to_redis(results, market_ctx, date_str)


# CLI entry point
if __name__ == "__main__":
    import argparse
    import sys

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(message)s",
    )

    parser = argparse.ArgumentParser(description="Publish screening results to Redis")
    parser.add_argument("json_file", nargs="?", default="combined_screening.json",
                        help="Path to screening JSON output file")
    args = parser.parse_args()

    publish_from_json_file(args.json_file)
