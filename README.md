# 📈 Stockbit Haka-Haki: Pipeline Trading Otomatis Go + Python

Sistem pipeline trading otomatis dan pembuat sinyal hibrida (Go & Python) yang tangguh untuk Bursa Efek Indonesia (BEI) menggunakan data dari Stockbit dan Yahoo Finance.

## 🌟 Sorotan Arsitektur

Proyek ini menggunakan arsitektur hibrida berbasis layanan mikro (*microservice*) demi kecepatan, keandalan, dan keamanan:
- **Go Engine (`/app`)**: Menangani tugas berkinerja tinggi seperti streaming data WebSocket *real-time*, Pembuatan Sinyal, Manajemen Risiko, Pelacakan Portofolio Virtual, dan Integrasi Analisis AI/LLM. Menggunakan TimescaleDB untuk penyimpanan tick/candle yang sangat cepat.
- **Python Screener (`/screener`)**: Melakukan scraping pasar, mencari saham likuid teratas, menghitung indikator teknikal lanjutan (seperti VWAP dari ketidakseimbangan pesanan *order-flow*), dan mengevaluasi sentimen indeks harga saham gabungan (IHSG).
- **Redis Sync**: Berperan sebagai sistem saraf penghubung. Go engine mengirimkan JWT Token Stockbit-nya ke Redis agar bisa dipakai oleh Python screener (mencegah login ganda). Screener juga mengirimkan insight pasar (IHSG Safety Gate, Top Watchlist) kembali ke Go engine secara seketika (*real-time*).

## ✨ Fitur Utama

### 🚀 Smart Bootstrap (Pemanasan Data Otomatis)
Jika database kosong saat sistem dijalankan, sistem akan otomatis melakukan 5 langkah *bootstrapping* tanpa intervensi manual:
1. Menarik saham likuid hari ini via Python Screener.
2. Menarik **Data Harian Jangka Panjang** (dari IPO hingga hari ini via Yahoo Finance) untuk MA200 dan garis tren jangka panjang.
3. Menarik **Data 5-Menit Jangka Pendek** (hingga 60 hari via Yahoo Finance) untuk Z-Score dan Deteksi Whale Intraday.
4. Menghitung standar statistik dasar (*baselines*) seperti VWAP, ATR, dan Volume Rata-rata.
5. Menjalankan Retrospeksi Whale untuk menganalisis aktivitas institusi sebelum hari ini.

### 💰 Manajer Portofolio Virtual
Mesin *mock-trading* terintegrasi untuk melacak kualitas sinyal secara realistis:
- Mensimulasikan saldo virtual (Default: Rp 200.000).
- Menentukan **Ukuran Posisi** (Lot Sizing) secara dinamis (Maksimal 10% per posisi, maksimal 70% total eksposur dari saldo).
- Menerapkan **Biaya Transaksi BEI Realistis** (0,15% Beli, 0,25% Jual termasuk pajak).
- Memberikan ringkasan Win Rate dan Profit/Loss (P/L) melalui `GET /api/portfolio`.

### 🛡️ Keamanan & Gerbang Keselamatan Kelas Institusi
- **Manajemen Token Terenkripsi**: Token JWT Stockbit disimpan menggunakan enkripsi AES-256-GCM.
- **Gerbang Keselamatan Pasar (Safety Gate)**: Python screener terus mengevaluasi indeks komposit IHSG. Jika pasar sedang jatuh (crash), Go engine menghentikan semua sinyal `BUY` sampai pasar stabil.
- **Batas Posisi Dinamis (Market Regime)**: 
  - `BULLISH`: 100% dari batas maksimal posisi.
  - `NEUTRAL`: 70% dari batas maksimal posisi.
  - `BEARISH`: 40% dari batas maksimal posisi.

### 🎯 Perlindungan *Drift* pada Stop Loss
- Melindungi *trailing stop* menggunakan *cache* `sync.Map` di memori. Memastikan bahwa batas kerugian (stop loss) hanya bisa naik menyesuaikan keuntungan, dan tidak pernah turun akibat fluktuasi kalkulasi saat pasar volatil.

---

## 📚 Indeks Dokumentasi

Untuk menjaga kesinambungan, silakan baca dokumentasi pendukung berikut sesuai dengan kebutuhan Anda:

1. 📖 **[Ringkasan Perubahan (CHANGELOG)](CHANGES_SUMMARY.md)** — Sejarah update fitur (Sesi 1, 2, 3), perubahan variabel *environment*, dan panduan migrasi.
2. 📖 **[Penjelasan Logika & Filter Sinyal](SIGNAL_IMPROVEMENTS.md)** — Penjelasan mendalam tentang bagaimana sinyal dibuat, mekanisme *Risk Management*, dan aturan *Exit Strategy* (Breakeven & Time-Decay).
3. 📖 **[Panduan Swing Trading](SWING_TRADING.md)** — Cara mengaktifkan mode hold overnight (sampai 30 hari) dan bedanya dengan Day Trading.
4. 📖 **[Dokumentasi Python Screener](screener/README.md)** — Penjelasan khusus tentang sub-sistem Python yang menganalisis IHSG dan mensuplai data watchlist.

---

## 🚀 Panduan Setup & Menjalankan Cepat

### 1. Persyaratan
Pastikan Anda memiliki:
- Docker & Docker Compose
- Make (opsional, tapi disarankan)
- Go 1.21+ (jika menjalankan lokal tanpa docker)
- Python 3.10+ (jika menjalankan screener lokal)

### 2. Konfigurasi Environment Variables
Salin file *environment* contoh dan isi nilainya:

```bash
cp .env.example .env
cp screener/.env.screener.example screener/.env.screener
```

**Konfigurasi Kunci di `.env`:**
```ini
# Keamanan (WAJIB DIISI!) - Masukkan 64 karakter hex string acak (32 bytes)
TOKEN_ENCRYPTION_KEY=MASUKKAN_64_KARAKTER_HEX_ANDA_DISINI

# Keamanan API (Otorisasi Endpoint)
API_KEY=KUNCI_RAHASIA_API_ANDA

# Kredensial Stockbit
STOCKBIT_USERNAME=username_anda
STOCKBIT_PASSWORD=password_anda
STOCKBIT_PLAYER_ID=player_id_anda

# Portofolio Virtual
TRADING_BALANCE=200000          # Saldo awal simulasi (Rp)
MAX_POSITION_PCT=10             # Maksimal % per posisi
MAX_TOTAL_EXPOSURE_PCT=70       # Maksimal total eksposur dari saldo
MOCK_TRADING_MODE=true          # Tetap 'true' untuk simulasi transaksi
```

### 3. Menjalankan Pipeline via Docker Compose
Untuk menyalakan Database, Redis, Go Engine, dan Python Screener secara bersamaan:

```bash
docker compose up -d
```

### 4. Alur Berjalannya Sistem
1. **Go Engine** menyala, login ke Stockbit, mengenkripsi *session*, dan menyebarkan JWT Token ke Redis.
2. **Go Engine** menjalankan **Smart Bootstrap** jika database `running_trades` kosong.
3. **Python Screener** bangun, mengambil JWT Token yang valid dari Redis (melewati batas *login manual*), dan mulai menganalisis IHSG serta saham likuid.
4. Screener mempublikasikan `ihsg:status` dan `watchlist:top` ke Redis.
5. **Go Engine** mendengarkan WebSocket Stockbit, dan mulai menyaring sinyal perdagangan dengan ketat terhadap *watchlist* dan *safety gate* dari screener.
6. **Manajer Portofolio Virtual** secara otomatis mencatat sinyal Beli/Jual, menghitung Win Rate dan P/L (Profit & Loss) yang sesungguhnya.

---

## 📊 Endpoints API & Pemantauan

Anda dapat mengakses API bawaan Go (Default: `http://localhost:8080`) untuk memantau status sistem:

- **Ringkasan Portofolio Virtual:** `GET /api/portfolio`
- **Status Smart Bootstrap:** `GET /api/bootstrap/status`
- **Posisi Terbuka (Open Positions):** `GET /api/positions/open`
- **Riwayat Sinyal:** `GET /api/signals/history`

Untuk mengubah state melalui endpoint (seperti POST/PUT), pastikan Anda menyertakan header otentikasi `X-API-Key: KUNCI_RAHASIA_API_ANDA` sesuai dengan `.env`.

---

## 📝 Lisensi
Proyek ini dibuat **hanya untuk tujuan edukasi dan riset**. Bukan merupakan saran keuangan (*not financial advice*).
