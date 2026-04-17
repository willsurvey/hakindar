# 🔄 Ringkasan Perubahan & Changelog (Stockbit Haka-Haki)

*Kembali ke [Halaman Utama](README.md)*

## 📑 Gambaran Umum

Sistem Analisis Whale Stockbit kini telah menerima peningkatan fitur besar-besaran untuk memastikan akurasi data, keandalan sistem hibrida (Go & Python), dan keamanan kelas institusi. Tiga pilar utama dalam pembaruan terbaru ini:
1. **Pipeline Data yang Andal (Smart Bootstrap)** - Sistem tidak lagi memulai dari nol, melainkan mengunduh data historis secara otomatis untuk *cold-start*.
2. **Manajer Portofolio Virtual** - Simulasi perdagangan nyata untuk mengukur kualitas sinyal dengan biaya transaksi realistis.
3. **Pengerasan Keamanan (Security Hardening)** - Proteksi API, enkripsi token, serta pencegahan SQL Injection.

---

## 📁 File yang Mengalami Perubahan Utama

### 1. Proses Bootstrapping (`app/smart_bootstrap.go`) - **[BARU]**
- Pipeline otomatis 5 langkah untuk mencegah basis data kosong.
- **Lapisan Ganda OHLCV**:
  - *Long-Term* (Harian): Mengambil data sejak IPO via Yahoo Finance (untuk MA200 & indikator jangka panjang).
  - *Short-Term* (5-Menit): Mengambil data intraday hingga 60 hari terakhir via Yahoo Finance.
- Menghitung standar *baseline* statistik otomatis.

### 2. Manajer Portofolio Virtual (`app/portfolio_manager.go`) - **[BARU]**
- Mesin *mock-trading* untuk menghitung Lot Size berdasarkan saldo virtual (`TRADING_BALANCE`).
- Menerapkan batasan risiko secara dinamis (contoh: maksimal 10% per posisi).
- Menerapkan biaya layanan BEI realistis: 0,15% untuk Beli, 0,25% untuk Jual.
- Menyediakan endpoint API `GET /api/portfolio`.

### 3. Keamanan Token & Relay Redis (`auth/auth.go`, `screener/screener.py`)
- Menerapkan enkripsi **AES-256-GCM** pada *file cache token*. Go Engine mempublikasikan token yang divalidasi ke Redis.
- Python Screener sekarang mengeksekusi "Langkah 0" (*Step 0*): Membaca JWT Stockbit yang sudah *live* dari Redis, sehingga tidak perlu melakukan proses *login Playwright* yang memakan waktu.

### 4. Perlindungan Basis Data & API (`api/server.go`, `database/repository.go`)
- Menyuntikkan lapisan `API_KEY` Middleware untuk memblokir mutasi yang tidak sah (`POST/PUT/DELETE`).
- Penerapan pola desain *whitelist* untuk operasi Hypertable, mencegah serangan injeksi SQL (SQL Injection).

### 5. Strategi Keluaran Jangka Panjang (`app/signal_tracker.go`)
- Memperbaiki perhitungan *VWAP* (Volume-Weighted Average Price) yang kini berbasis asimetri pergerakan *order-flow*.
- Menambahkan lapisan *Cache* di memori (`sync.Map`) untuk mengunci *Trailing Stop*. Tujuannya mencegah *trailing stop* turun ketika ada fluktuasi dalam perhitungan *ATR*.

---

## 🎯 Perubahan Aturan & Konfigurasi (*Breaking Changes*)

### Penambahan Variabel Environment (`.env`)
Untuk menunjang fitur-fitur di atas, kami menambahkan pengaturan khusus di lingkungan lokal (`.env`):
```bash
# Untuk Portofolio
TRADING_BALANCE=200000          # Saldo simulasi
MAX_POSITION_PCT=10             # Limit % per posisi
MAX_TOTAL_EXPOSURE_PCT=70       # Limit total paparan posisi aktif

# Untuk Keamanan
TOKEN_ENCRYPTION_KEY=...        # Wajib (64-character hex)
API_KEY=...                     # Kunci proteksi API

# Konfigurasi Swing Trading (Jika Anda Ingin Hold Overnight)
SWING_TRADING_ENABLED=true
SWING_MIN_CONFIDENCE=0.75
```

### Penyesuaian Algoritma Filter
Filter yang tadinya menolak sinyal secara instan (misal: "waktu bukan jam trading", atau "order flow rendah") sekarang diubah menjadi model berbasis **Probabilitas Multiplier**. Artinya, sistem tidak menolak mentah-mentah, melainkan menurunkan persentase kelayakannya. Hal ini menaikkan jumlah peluang emas yang sebelumnya terlewat karena filter statis.

*(Untuk penjelasan teknis lebih detail tentang bagaimana algoritma memfilter sinyal, lihat [Logika Perbaikan Sinyal](SIGNAL_IMPROVEMENTS.md)).*

---

## ✅ Panduan Migrasi (Bagi Pengguna Lama)

1. **Tambahkan Variabel Wajib di `.env`:**
   Pastikan Anda menyalin blok konfigurasi baru dari `.env.example`, khususnya `TOKEN_ENCRYPTION_KEY`. Jika tidak, Go Engine tidak bisa *start*.

2. **Periksa Dashboard Anda:**
   Sekarang Anda bisa melihat apakah sistem sudah berjalan normal melalui dua endpoint baru ini:
   - `GET /api/bootstrap/status` (Untuk memantau persentase *download* data historis di 5 menit pertama)
   - `GET /api/portfolio` (Untuk melihat simulasi keuntungannya).

3. **Coba Aktifkan Swing Trading:**
   Jika sistem Day Trading dirasa belum maksimal, pelajari [Panduan Swing Trading](SWING_TRADING.md) untuk membiarkan posisi menggantung (hold) hingga berhari-hari.
