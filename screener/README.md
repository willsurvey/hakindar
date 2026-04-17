# Python Screener Engine

Screener Python ini bertugas untuk melakukan komputasi berat (Pandas/NumPy) dan scraping (Playwright) untuk mendukung Go Engine utama.

## 🌟 Fitur Utama
1. **IHSG Safety Gate**: Menganalisis sentimen komposit IHSG. Jika pasar crash/bearish, screener memberitahu Go Engine untuk menghentikan aksi `BUY`.
2. **Top Liquid Watchlist**: Menarik data saham-saham paling likuid yang sedang bergerak naik untuk menjadi fokus utama pemantauan WebSocket Go.
3. **Redis Token Relay (Step 0 Auth)**: Tidak perlu repot login Playwright jika Go Engine sedang berjalan! Screener akan membaca JWT token Stockbit dari `Redis` secara otomatis yang di-publish oleh Go Engine.

## 🚀 Cara Menjalankan

### Via Docker Compose (Rekomendasi)
Dari root project, jalankan:
```bash
docker compose up -d screener
```

### Jalankan Langsung (Local Development)
Pastikan Anda memiliki Redis berjalan di lokal (`localhost:6379`), lalu:

```bash
cd screener
pip install -r requirements.txt
python screener.py
```

## ⚙️ Konfigurasi (`.env.screener`)
Salin `.env.screener.example` menjadi `.env.screener` dan atur isinya. 

| Variabel | Penjelasan |
|---|---|
| `SCREENER_MODE` | `A` = Market Mover (Stockbit), `B` = Yahoo Finance Fallback |
| `SCREENER_MAX_OUTPUT` | Jumlah maksimal watchlist saham (misal: `5`) |
| `REDIS_HOST` | Host Redis (default `localhost` atau `redis` di Docker) |
| `REDIS_PORT` | Port Redis (default `6379`) |

*Catatan: Kredensial Stockbit (username/password) hanya diperlukan jika Redis mati dan screener dipaksa untuk login ulang secara mandiri via Playwright.*

## 📡 Output Integrasi (Redis)

Hasil komputasi akan di-publish ke Redis setiap beberapa menit agar bisa dikonsumsi seketika oleh Go Engine:
- Key: `watchlist:top` -> Berisi daftar saham likuid.
- Key: `ihsg:status` -> Berisi status pasar (`BULLISH`, `NEUTRAL`, `BEARISH`).
- Menyimpan hasil mentah ke: `/app/output/latest_screening.json` (di dalam container).
