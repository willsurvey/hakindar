#!/usr/bin/env python3
"""
Redis Reader Module — Baca data yang sudah di-fetch oleh Go StockbitCollector.

Modul ini menyediakan helper functions untuk membaca data dari Redis
yang di-push oleh Go engine (realtime/stockbit_collector.go).

Redis Key Convention (diisi oleh Go):
    stockbit:universe:mover       → MarketMoverItem[] (TOP_VOLUME)
    stockbit:universe:top_value   → MarketMoverItem[] (TOP_VALUE)
    stockbit:universe:foreign     → MarketMoverItem[] (NET_FOREIGN_BUY)
    stockbit:universe:gainer      → MarketMoverItem[] (TOP_GAINER)
    stockbit:universe:loser       → MarketMoverItem[] (TOP_LOSER)
    stockbit:universe:guru:{id}   → string[] (ticker list)
    stockbit:hist:{TICKER}        → HistoricalSummary
    stockbit:broker:{TICKER}      → BrokerSignal
    stockbit:orderbook:{TICKER}   → Orderbook
    stockbit:keystats:{TICKER}    → KeystatsData
    stockbit:running_trade        → RunningTradeSummary
"""

import json
import logging
import os
from typing import Dict, List, Optional, Tuple

log = logging.getLogger(__name__)

# Guru screener template IDs (matches Go guruTemplates)
GURU_TEMPLATE_IDS = [92, 77, 94, 87, 88, 63, 97, 72, 78, 79]


def _get_redis_client():
    """Create Redis connection from environment variables."""
    try:
        import redis
    except ImportError:
        log.warning("redis package not installed — Redis reader unavailable")
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
        return client
    except Exception as e:
        log.warning(f"⚠️ Redis connection failed: {e}")
        return None


def _read_json(client, key) -> Optional[Dict]:
    """Read and parse a JSON value from Redis."""
    try:
        raw = client.get(key)
        if raw:
            return json.loads(raw)
    except Exception as e:
        log.debug(f"Redis read {key}: {e}")
    return None


# =============================================================================
# Universe readers
# =============================================================================

def read_universe_mover() -> List[Dict]:
    """Read market mover universe (TOP_VOLUME) from Redis."""
    client = _get_redis_client()
    if not client:
        return []
    try:
        data = _read_json(client, "stockbit:universe:mover")
        return data if isinstance(data, list) else []
    finally:
        client.close()


def read_universe_gainer() -> List[Dict]:
    """Read top gainer universe from Redis."""
    client = _get_redis_client()
    if not client:
        return []
    try:
        data = _read_json(client, "stockbit:universe:gainer")
        return data if isinstance(data, list) else []
    finally:
        client.close()


def read_universe_loser() -> List[Dict]:
    """Read top loser universe from Redis."""
    client = _get_redis_client()
    if not client:
        return []
    try:
        data = _read_json(client, "stockbit:universe:loser")
        return data if isinstance(data, list) else []
    finally:
        client.close()


def read_universe_top_value() -> List[Dict]:
    """Read top value universe from Redis."""
    client = _get_redis_client()
    if not client:
        return []
    try:
        data = _read_json(client, "stockbit:universe:top_value")
        return data if isinstance(data, list) else []
    finally:
        client.close()


def read_universe_foreign() -> List[Dict]:
    """Read net foreign buy universe from Redis."""
    client = _get_redis_client()
    if not client:
        return []
    try:
        data = _read_json(client, "stockbit:universe:foreign")
        return data if isinstance(data, list) else []
    finally:
        client.close()


def read_universe_guru(template_id: int) -> List[str]:
    """Read guru screener ticker list from Redis."""
    client = _get_redis_client()
    if not client:
        return []
    try:
        data = _read_json(client, f"stockbit:universe:guru:{template_id}")
        return data if isinstance(data, list) else []
    finally:
        client.close()


def read_all_universe() -> Dict[str, List]:
    """
    Read all universe data from Redis in one connection.
    Returns dict with keys: mover, gainer, loser, top_value, foreign, guru_{id}.
    """
    client = _get_redis_client()
    if not client:
        return {}
    try:
        result = {}
        for key_suffix in ["mover", "gainer", "loser", "top_value", "foreign"]:
            data = _read_json(client, f"stockbit:universe:{key_suffix}")
            result[key_suffix] = data if isinstance(data, list) else []

        for tid in GURU_TEMPLATE_IDS:
            data = _read_json(client, f"stockbit:universe:guru:{tid}")
            result[f"guru_{tid}"] = data if isinstance(data, list) else []

        return result
    finally:
        client.close()


# =============================================================================
# Per-symbol readers
# =============================================================================

def read_historical(ticker: str) -> Optional[Dict]:
    """
    Read historical summary (20-day OHLCV + foreign flow) from Redis.

    Returns dict with keys: ticker, bars[], updated_at
    Each bar has: date, open, high, low, close, volume, frequency, value,
                  foreign_buy, foreign_sell, net_foreign, change_pct
    """
    client = _get_redis_client()
    if not client:
        return None
    try:
        return _read_json(client, f"stockbit:hist:{ticker}")
    finally:
        client.close()


def read_broker_signal(ticker: str) -> Optional[Dict]:
    """
    Read broker signal (bandar detector) from Redis.

    Returns dict with keys: ticker, label, score, updated_at
    label is one of: Big Acc, Acc, Neutral, Dist, Big Dist
    """
    client = _get_redis_client()
    if not client:
        return None
    try:
        return _read_json(client, f"stockbit:broker:{ticker}")
    finally:
        client.close()


def read_orderbook(ticker: str) -> Optional[Dict]:
    """
    Read orderbook (bid/ask walls) from Redis.

    Returns dict with keys: ticker, bids[], asks[], updated_at
    Each level has: price, lot
    """
    client = _get_redis_client()
    if not client:
        return None
    try:
        return _read_json(client, f"stockbit:orderbook:{ticker}")
    finally:
        client.close()


def read_keystats(ticker: str) -> Optional[Dict]:
    """
    Read fundamental keystats from Redis.

    Returns dict with keys: symbol, pe_ttm, eps_ttm, roe_ttm, roa_ttm,
    net_profit_margin, revenue_growth_yoy, net_income_growth_yoy,
    dividend_yield, piotroski_score, high_52w, low_52w, price_return_ytd,
    debt_to_equity, ev_ebitda, pbv, updated_at
    """
    client = _get_redis_client()
    if not client:
        return None
    try:
        return _read_json(client, f"stockbit:keystats:{ticker}")
    finally:
        client.close()


def read_running_trade() -> Optional[Dict]:
    """
    Read running trade summary from Redis.

    Returns dict with keys: timestamp, date, total_symbols, summary
    summary is a dict of {symbol: {buy_lot, sell_lot, net_lot, foreign_net, dominant_buyer, tx_count}}
    """
    client = _get_redis_client()
    if not client:
        return None
    try:
        return _read_json(client, "stockbit:running_trade")
    finally:
        client.close()


# =============================================================================
# Batch readers (single connection for multiple symbols)
# =============================================================================

def read_historical_batch(tickers: List[str]) -> Dict[str, Dict]:
    """Read historical data for multiple tickers in one connection."""
    client = _get_redis_client()
    if not client:
        return {}
    try:
        result = {}
        for ticker in tickers:
            data = _read_json(client, f"stockbit:hist:{ticker}")
            if data:
                result[ticker] = data
        return result
    finally:
        client.close()


def read_broker_signal_batch(tickers: List[str]) -> Dict[str, Dict]:
    """Read broker signals for multiple tickers in one connection."""
    client = _get_redis_client()
    if not client:
        return {}
    try:
        result = {}
        for ticker in tickers:
            data = _read_json(client, f"stockbit:broker:{ticker}")
            if data:
                result[ticker] = data
        return result
    finally:
        client.close()


def read_orderbook_batch(tickers: List[str]) -> Dict[str, Dict]:
    """Read orderbooks for multiple tickers in one connection."""
    client = _get_redis_client()
    if not client:
        return {}
    try:
        result = {}
        for ticker in tickers:
            data = _read_json(client, f"stockbit:orderbook:{ticker}")
            if data:
                result[ticker] = data
        return result
    finally:
        client.close()


# =============================================================================
# Health check
# =============================================================================

def check_redis_data_available() -> bool:
    """
    Check if Go collector has populated Redis with data.
    Returns True if at least one universe key exists.
    """
    client = _get_redis_client()
    if not client:
        return False
    try:
        for key in ["stockbit:universe:mover", "stockbit:universe:gainer"]:
            if client.exists(key):
                return True
        return False
    finally:
        client.close()
