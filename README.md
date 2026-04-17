# 📈 Stockbit Haka-Haki: Go + Python Automated Trading Pipeline

A robust, hybrid Go & Python automated trading pipeline and signal generation engine for the Indonesian Stock Exchange (IDX) using Stockbit and Yahoo Finance data.

## 🌟 Architectural Highlights

This project adopts a microservice-oriented hybrid architecture for speed, reliability, and security:
- **Go Engine (`/app`)**: Handles high-performance tasks including real-time WebSocket data streaming, Signal Generation, Risk Management, Virtual Portfolio tracking, and LLM Analysis integration. Uses TimescaleDB for hyperfast tick/candle storage.
- **Python Screener (`/screener`)**: Scrapes the market, identifies top liquid movers, calculates advanced technicals (like VWAP from order-flow imbalances), and evaluates the composite index (IHSG).
- **Redis Sync**: Acts as the nervous system. The Go engine relays its active Stockbit JWT Token to Redis, which the Python screener consumes (preventing duplicate logins). The screener relays its market insights (IHSG Safety Gate, Top Watchlist) back to the Go engine in real-time.

## ✨ Core Features

### 🚀 Smart Bootstrap (Cold-Start Auto-Healing)
If the database is empty, the system automatically runs a 5-step bootstrapping process without manual intervention:
1. Fetch today's liquid stocks via Python Screener.
2. Fetch **Long-Term Daily Data** (from IPO to present via Yahoo Finance) for MA200 and long-term trend lines.
3. Fetch **Short-Term 5-min Data** (up to 60 days via Yahoo Finance) for Z-Score and Intraday Whale Detection.
4. Calculate statistical baselines (VWAP, ATR, Mean Volume).
5. Run Whale Retrospective to analyze institutional activity prior to today.

### 💰 Virtual Portfolio Manager
An integrated mock-trading engine to track signal quality realistically:
- Simulates a configurable balance (Default: Rp 200,000).
- Determines **Position Sizing** dynamically (Max 10% per position, max 70% total exposure).
- Calculates exact Lot Sizes (1 lot = 100 shares).
- Applies **Realistic IDX Fees** (0.15% Buy, 0.25% Sell including tax).
- Offers a clear snapshot of Realized P/L and Win Rate via `GET /api/portfolio`.

### 🛡️ Institutional-Grade Security & Safety Gates
- **Encrypted Token Management**: Stockbit JWT tokens are stored using AES-256-GCM encryption.
- **Market Safety Gate**: The Python screener continuously evaluates the IHSG composite index. If the market is crashing, the Go engine halts all buying (Safety Gate), ignoring bullish signals until the market stabilizes.
- **Dynamic Position Limit via Market Regime**: 
  - `BULLISH`: 100% of max allowed position size.
  - `NEUTRAL`: 70% of max allowed position size.
  - `BEARISH`: 40% of max allowed position size.

### 🎯 Exit Levels & Drift Protection
- Protects trailing stops using in-memory `sync.Map` cache. Ensures that stop losses are ratcheted upwards but never downwards during volatile recalculations.

## 🛠️ Tech Stack
- **Go 1.21+**: Concurrency, Websockets, API, Database Management.
- **Python 3.10+**: Playwright scraping, Pandas/Numpy heavy calculations.
- **TimescaleDB / PostgreSQL**: Time-series optimized hyper-tables.
- **Redis**: Token relay, message brokering, watchlist synchronization, and caching.
- **Docker Compose**: Entire stack is containerized.

---

## 🚀 Quick Start & Setup Instructions

### 1. Requirements
Ensure you have the following installed:
- Docker & Docker Compose
- Make (optional, but recommended for shortcuts)
- Go 1.21+ (if running locally without docker)
- Python 3.10+ (if running screener locally)

### 2. Environment Variables Configuration
Clone the repository and copy the example environment files:

```bash
cp .env.example .env
cp screener/.env.screener.example screener/.env.screener
```

**Key configurations in `.env`:**
```ini
# Stockbit Credentials
STOCKBIT_USERNAME=your_username
STOCKBIT_PASSWORD=your_password
STOCKBIT_PLAYER_ID=your_player_id

# Security (MUST BE SET!) - Provide a 64 character hex string (32 bytes)
TOKEN_ENCRYPTION_KEY=YOUR_64_CHAR_HEX_STRING_HERE

# API Security
API_KEY=YOUR_SECRET_API_KEY_FOR_SECURE_ENDPOINTS

# Virtual Portfolio
TRADING_BALANCE=200000          # Initial mock balance (Rp)
MAX_POSITION_PCT=10             # Max % per position
MAX_TOTAL_EXPOSURE_PCT=70       # Max total exposure of your balance
MOCK_TRADING_MODE=true          # Keep true for simulated trades
```

### 3. Start the Pipeline via Docker Compose
To boot up the database, Redis, the Go Engine, and the Python Screener simultaneously:

```bash
docker compose up -d
```

### 4. How it Runs
1. The **Go Engine** starts, authenticates with Stockbit, encrypts and saves its session, and publishes the JWT to Redis.
2. The **Go Engine** triggers **Smart Bootstrap** if the `running_trades` database is empty.
3. The **Python Screener** wakes up, grabs the valid JWT from Redis (bypassing the need to log in again), and begins analyzing IHSG and top liquid stocks.
4. The Screener publishes the `ihsg:status` and `watchlist:top` to Redis.
5. The **Go Engine** listens to Stockbit WebSockets, applying strict filtering against the screener's watchlist and IHSG safety gates.
6. The **Virtual Portfolio** automatically tracks Buy/Sell signals, calculating realistic Win Rates and P&L.

---

## 📊 Endpoints & Monitoring

You can access the built-in Go API (Default: `http://localhost:8080`) to monitor system status:

- **Portfolio Snapshot:** `GET /api/portfolio`
- **Smart Bootstrap Status:** `GET /api/bootstrap/status`
- **Active Open Positions:** `GET /api/positions/open`
- **Signal History:** `GET /api/signals/history`

For modifying endpoints (requires the API_KEY set in `.env`):
- Include the header `X-API-Key: YOUR_API_KEY`.

---

## 📚 Project Structure

```text
.
├── api/                  # REST API server & middleware (JWT, API keys)
├── app/                  # Go Engine core: smart bootstrap, signal trackers, portfolio
├── auth/                 # AES-GCM Token lifecycle & encryption
├── cache/                # Redis interface
├── config/               # ENV loading
├── database/             # PostgreSQL / Timescale models & repositories
├── docs/                 # Extended documentation
├── integration/          # Redis pub/sub bridges (Watchlist Sync)
├── realtime/             # Server-Sent Events (SSE) broker
├── screener/             # Python Screener (Playwright, Pandas, Redis Publisher)
└── docker-compose.yml    # Full stack orchestration
```

## 📝 License
This project is for educational purposes only. Not for financial advice.
